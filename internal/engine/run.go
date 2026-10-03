package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Options configures one finite execution of the CLI.
type Options struct {
	ConfigPath, CacheDir string
	Method               string
	Resources            []string
	// ChangedSince selects, for prepare, the resources whose preparation
	// differs from the catalog at this Git revision, after checking the whole
	// lockfile.
	ChangedSince string
	Lock         lockfile.Options
	Icons        IconOptions
	// Input selects artwork or facts without building the selected resource.
	Input InputSelection
	// Output names the resource output the artifact method materializes;
	// empty selects installer.
	Output string
	// OutputFile exports the selected artifact outside the disposable cache.
	OutputFile string
	Handlers   map[string]reconcileHandler
	// ResourceDone receives each final resource result, including failures.
	ResourceDone func(ResourceReport) error
}

// Run resolves locked resources in dependency order and reconciles destinations independently.
func Run(ctx context.Context, opts Options) (report Report, runErr error) {
	defer func() {
		report.Summarize(opts.Method)
		if runErr != nil {
			report.Error = runErr.Error()
		}
	}()
	switch opts.Method {
	case "update", "prepare", "signature", "plan", "apply", "icon":
	case "artifact", "inspect":
		if len(opts.Resources) != 1 {
			return report, fmt.Errorf("%s requires one resource selector", opts.Method)
		}
	default:
		return report, fmt.Errorf("unsupported run method %q", opts.Method)
	}
	if opts.ChangedSince != "" && (opts.Method != "prepare" || len(opts.Resources) > 0) {
		return report, errors.New("changed-since selects the resources prepare runs; remove the selectors")
	}
	if opts.Method == "icon" {
		// A presentation the host cannot draw fails before anything is acquired.
		presentation, err := opts.Icons.Presentation.Resolve()
		if err != nil {
			return report, err
		}
		opts.Icons.Presentation = presentation
	}
	// Update writes the lockfile and resolves plugin declarations. Every other
	// run consumes it as reviewed.
	opts.Lock.Refresh = opts.Method == "update"
	s, err := open(ctx, opts, opts.Lock.Refresh)
	if err != nil {
		return report, err
	}
	defer s.close(ctx)
	e := &execution{
		opts: opts, session: s, report: &report,
		prepared: map[string]preparedResource{}, pending: map[string]int{},
		reconciled: map[destinationRef]bool{}, failed: map[destinationRef]bool{},
	}
	// Workspaces go before the session releases its cache lease.
	defer e.removeWorkspaces(ctx)
	if err := e.begin(ctx); err != nil {
		return report, err
	}
	report.Resources = []ResourceReport{}
	if opts.Method != "update" {
		for destination := range e.destinations {
			e.pending[destination.Resource]++
		}
	}
	switch opts.Method {
	case "icon":
		err = e.icons(ctx)
	case "inspect":
		err = e.inspectInput(ctx)
	case "artifact":
		err = e.materialize(ctx)
	default:
		err = e.run(ctx)
	}
	return report, errors.Join(append(e.failures, err)...)
}

// execution is one run over its selected resources. It owns the report and
// what each resource reached: its preparation, which destinations and icons
// consume, and its destinations' outcomes.
type execution struct {
	opts    Options
	session *session
	report  *Report

	// roots are the resources the run selected; selected is their closure in
	// dependency order, and plans describes each resource in it.
	roots, selected []string
	plans           map[string]resourcePlan
	// destinations are the selected publications and connections their
	// evaluated settings, which only plan and apply read.
	destinations map[destinationRef]destinationPlan
	connections  map[string]json.RawMessage
	locked       *lockfile.Update
	inputOwner   string

	prepared map[string]preparedResource
	// pending counts each resource's destinations still to reconcile; the
	// resource completes when none remain.
	pending            map[string]int
	reconciled, failed map[destinationRef]bool
	workdirs           []string
	// failures are the resource failures the run returns.
	failures []error
}

type preparedResource struct {
	work    string
	outputs map[string]Prepared
	report  int
	ready   bool
}

func (e *execution) publishing() bool {
	return e.opts.Method == "plan" || e.opts.Method == "apply"
}

// begin selects the run's resources, checks every contract they rely on and
// opens the lockfile, before anything is acquired.
func (e *execution) begin(ctx context.Context) (err error) {
	s := e.session
	done := plugin.Stage(ctx, "Validating operation contracts")
	defer func() { done(err) }()
	e.roots, err = selectResources(s.project.Resources, e.opts.Resources)
	if err != nil {
		return err
	}
	if err := registerResolvers(s.manager, s.ops, s.work); err != nil {
		return err
	}
	if e.opts.ChangedSince != "" {
		if e.roots, err = changedSince(ctx, s, e.opts.ChangedSince); err != nil {
			return err
		}
	}
	// Updates, artifacts and icons never reach a destination, so only the
	// other runs check the destinations they publish to.
	usesDestinations := e.opts.Method != "update" && e.opts.Method != "artifact" && e.opts.Method != "icon" && e.opts.Method != "inspect"
	e.plans, e.selected, err = discoverClosure(ctx, s.project, s.ops, e.roots, true, usesDestinations)
	if err != nil {
		return err
	}
	if err := preflight(e.plans, e.selected, s.project, s.ops, usesDestinations); err != nil {
		return err
	}
	if e.opts.Method == "inspect" && e.opts.Input.Name == "" {
		return errors.New("inspect requires an input name")
	}
	if e.opts.Input.Path != "" && e.opts.Input.Name == "" {
		return errors.New("path requires an input")
	}
	if e.opts.Input.Name != "" {
		if len(e.roots) != 1 {
			return errors.New("input requires one resource selector")
		}
		if e.opts.Method == "icon" && e.plans[e.roots[0]].Icon == "" {
			return errors.New("selected resource must declare an icon")
		}
		e.inputOwner, err = inputOwner(e.plans, e.roots[0], e.opts.Input.Name)
		if err != nil {
			return err
		}
	}
	if e.publishing() {
		// Locking and icon creation come first for a new declaration, so only
		// publication requires the asset.
		if err := verifyIcons(s.root, e.plans, e.selected); err != nil {
			return err
		}
	}
	e.destinations = map[destinationRef]destinationPlan{}
	if usesDestinations {
		if e.destinations, err = planDestinations(ctx, s.project, e.plans, s.ops, s.root, e.selected, e.publishing()); err != nil {
			return err
		}
	}
	// Publication connects with evaluated settings, so a missing value fails
	// before anything is acquired.
	e.connections = map[string]json.RawMessage{}
	if e.publishing() {
		for node := range e.destinations {
			e.connections[node.Destination] = nil
		}
		for _, name := range sortedKeys(e.connections) {
			if e.connections[name], err = connectionSettings(s.ops, name, s.project.Destinations[name]); err != nil {
				return err
			}
		}
	}
	done(nil)
	inputs := declarations(e.plans, e.selected)
	opts := e.opts.Lock
	opts.PreserveUnselected = len(e.opts.Resources) > 0 || e.opts.ChangedSince != ""
	opts.Retain = suspended(s.project.Resources)
	plugin.Logger(ctx).DebugContext(ctx, "Resources selected", "count", len(e.selected), "suspended", len(opts.Retain))
	e.locked, err = lockfile.Begin(ctx, s.root, inputs, s.ops.plugins, s.manager, opts)
	return err
}

// run prepares every selected resource and reconciles its destinations,
// then commits the lock changes of the resources that succeeded.
func (e *execution) run(ctx context.Context) error {
	for _, key := range e.selected {
		if e.opts.Method == "update" || len(e.plans[key].Destinations) == 0 {
			if err := e.prepare(ctx, key); err != nil {
				return err
			}
		}
		if e.opts.Method == "update" {
			continue
		}
		for _, destination := range sortedKeys(e.plans[key].Destinations) {
			if err := e.reconcile(ctx, destinationRef{key, destination}); err != nil {
				return err
			}
		}
	}
	return e.commit(ctx)
}

// commit records the lock changes of the resources that succeeded; a rejected
// resource keeps its reviewed entries.
func (e *execution) commit(ctx context.Context) error {
	var rejected []string
	for _, resource := range e.report.Resources {
		if resource.Error != "" {
			rejected = append(rejected, resource.Key)
		}
	}
	result, err := e.locked.Commit(ctx, rejected...)
	if err != nil {
		return err
	}
	if e.opts.Method == "update" {
		e.report.LockChanged = &result.Changed
		e.report.Plugins = result.Plugins
	}
	reported := map[string]bool{}
	for _, resource := range e.report.Resources {
		reported[resource.Key] = true
	}
	for _, change := range result.Changes {
		if !reported[change.Resource] {
			e.report.RemovedInputs = append(e.report.RemovedInputs, change)
		}
	}
	return nil
}

// icons creates each selected resource's missing or forced icon. Only a
// missing or forced asset costs a preparation, so a run across the catalog is
// cheap; creating the icon completes the resource in place of publication.
func (e *execution) icons(ctx context.Context) error {
	if e.opts.Input.Name != "" {
		return e.inputIcon(ctx)
	}
	outcomes, building := map[string]string{}, map[string]bool{}
	var builds func(string)
	builds = func(key string) {
		for _, input := range e.plans[key].Inputs {
			if ref := input.Resource; ref != nil && !building[ref.Key()] {
				building[ref.Key()] = true
				builds(ref.Key())
			}
		}
	}
	for _, key := range e.selected {
		if outcomes[key] = iconOutcome(e.opts.Icons, e.session.root, e.plans[key]); outcomes[key] == "" {
			e.pending[key] = 1
			builds(key)
		}
	}
	for _, key := range e.selected {
		if outcome := outcomes[key]; outcome != "" {
			if building[key] {
				continue // Preparing the resource that consumes it reports it.
			}
			resource := e.plans[key].Resource
			e.report.Resources = append(e.report.Resources, ResourceReport{Name: resource.Metadata.Name, Kind: resource.Kind, Key: key, Icon: outcome})
			if err := e.complete(ctx, &e.report.Resources[len(e.report.Resources)-1]); err != nil {
				return err
			}
			continue
		}
		if err := e.prepare(ctx, key); err != nil {
			return err
		}
		prepared := e.prepared[key]
		if !prepared.ready {
			continue
		}
		item := &e.report.Resources[prepared.report]
		var err error
		item.Icon, err = createIcon(resourceContext(ctx, e.plans[key].Resource), e.opts.Icons, e.session.root, e.plans[key], prepared.outputs["installer"], "", prepared.work)
		if err != nil {
			item.Error = err.Error()
			e.fail(ctx, ResourceError{Resource: key, Err: err})
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.complete(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

// materialize prepares the selected resource and the builds it consumes from
// the reviewed lockfile and exposes the selected output. No destination sees
// the prepared artifact.
func (e *execution) materialize(ctx context.Context) error {
	key := e.roots[0]
	e.pending[key] = 1
	if err := e.prepare(ctx, key); err != nil {
		return err
	}
	prepared := e.prepared[key]
	if !prepared.ready {
		return nil
	}
	item := &e.report.Resources[prepared.report]
	output := cmp.Or(e.opts.Output, "installer")
	artifact, ok := prepared.outputs[output]
	var err error
	if ok {
		e.report.Artifact, err = expose(resourceContext(ctx, e.plans[key].Resource), e.session.store, artifact, filepath.Join(prepared.work, "materialized"), e.opts.OutputFile)
	} else {
		err = fmt.Errorf("no %s output; the resource prepares %s", output, strings.Join(sortedKeys(prepared.outputs), ", "))
	}
	if err != nil {
		item.Error = err.Error()
		e.fail(ctx, ResourceError{Resource: key, Err: err})
	}
	return e.complete(ctx, item)
}

// prepare prepares a resource once, after the resources whose outputs it
// consumes, and reports it. A preparation failure belongs to the resource; the
// error is one that stops the run.
func (e *execution) prepare(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, exists := e.prepared[key]; exists {
		return nil
	}
	plan := e.plans[key]
	for _, name := range sortedKeys(plan.Inputs) {
		if ref := plan.Inputs[name].Resource; ref != nil {
			if err := e.prepare(ctx, ref.Key()); err != nil {
				return err
			}
		}
	}
	ctx = resourceContext(ctx, plan.Resource)
	plugin.Logger(ctx).DebugContext(ctx, "Executing resource")
	started := time.Now()
	item := ResourceReport{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Key: key}
	for _, producer := range producers(plan) {
		if !e.prepared[producer].ready {
			item.BlockedBy = append(item.BlockedBy, producer)
		}
	}
	var entries map[string]source.Entry
	var failure error
	if len(item.BlockedBy) > 0 {
		failure = fmt.Errorf("blocked by %s", strings.Join(item.BlockedBy, ", "))
	} else {
		entries, item.InputCacheHits, failure = e.locked.Acquire(ctx, key)
	}
	result := preparedResource{report: len(e.report.Resources)}
	if failure == nil && e.opts.Method != "update" {
		work, err := os.MkdirTemp(filepath.Join(e.session.store.Dir, "work"), "resource-*")
		if err != nil {
			return err
		}
		e.workdirs = append(e.workdirs, work)
		result.work = work
		var inputs map[string]Prepared
		if inputs, failure = e.inputs(plan, entries); failure == nil {
			derive := ""
			if e.opts.Method == "signature" {
				derive = "signature"
			}
			result.outputs, item.Cached, failure = prepareResource(ctx, e.session.store, e.session.ops, plan, inputs, work, derive)
		}
	}
	item.Artifacts = result.outputs
	result.ready = failure == nil
	if failure != nil {
		item.Error = failure.Error()
		if len(item.BlockedBy) == 0 {
			e.fail(ctx, ResourceError{Resource: key, Err: failure})
		}
	}
	e.prepared[key] = result
	e.report.Resources = append(e.report.Resources, item)
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case failure == nil && e.opts.Method == "update":
		plugin.Logger(ctx).DebugContext(ctx, "Inputs resolved", "elapsed", time.Since(started).Round(time.Millisecond))
	case failure == nil:
		artifact := result.outputs["installer"]
		plugin.Logger(ctx).DebugContext(ctx, "Prepared", "artifact", artifact.Filename, "version", artifact.Version, "cached", item.Cached, "elapsed", time.Since(started).Round(time.Millisecond))
	case len(item.BlockedBy) > 0:
		plugin.Logger(ctx).DebugContext(ctx, "Resource blocked", "blocked_by", item.BlockedBy)
	default:
		plugin.Logger(ctx).DebugContext(ctx, "Preparation failed", "error", failure)
	}
	if failure != nil || e.pending[key] == 0 {
		return e.complete(ctx, &e.report.Resources[result.report])
	}
	return nil
}

func (e *execution) inputs(plan resourcePlan, entries map[string]source.Entry) (map[string]Prepared, error) {
	inputs := map[string]Prepared{}
	for _, name := range sortedKeys(plan.Inputs) {
		input, err := e.input(plan.Inputs[name], entries[name])
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", name, err)
		}
		inputs[name] = input
	}
	return inputs, nil
}

func (e *execution) input(input plugin.Input, entry source.Entry) (Prepared, error) {
	if input.Resource == nil {
		ref, err := e.session.store.Lookup(entry.Content.SHA256)
		if err != nil {
			return Prepared{}, err
		}
		return Prepared{Payload: ref, Filename: entry.Content.Filename, Tree: entry.Content.Tree, Mode: entry.Content.Mode, Version: entry.InputVersion, ContentRoot: entry.ContentRoot, InputsHash: entry.Content.SHA256, Evidence: entry.Evidence}, nil
	}
	ref := input.Resource
	producer := e.prepared[ref.Key()]
	output := cmp.Or(ref.Output, "installer")
	artifact, ok := producer.outputs[output]
	if !producer.ready || !ok {
		return Prepared{}, fmt.Errorf("requires successful %s output %s", ref.Key(), output)
	}
	return artifact, nil
}

// complete reports a finished resource. A rejected resource keeps its
// reviewed entries, so only a successful one has input changes.
func (e *execution) complete(ctx context.Context, item *ResourceReport) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if item.Error == "" {
		item.Inputs = e.locked.Changes(item.Key)
	}
	if e.opts.ResourceDone != nil {
		if err := e.opts.ResourceDone(*item); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// fail records a resource failure, unless the run was interrupted and the
// failure is only its echo.
func (e *execution) fail(ctx context.Context, err ResourceError) {
	if ctx.Err() == nil {
		e.failures = append(e.failures, err)
	}
}

func (e *execution) removeWorkspaces(ctx context.Context) {
	if len(e.workdirs) == 0 {
		return
	}
	done := plugin.Stage(ctx, "Removing workspaces")
	var err error
	for _, work := range e.workdirs {
		err = errors.Join(err, os.RemoveAll(work))
	}
	done(err)
	cleanupError(ctx, err)
}

// resourceContext scopes logs and terminal progress to one resource tree.
func resourceContext(ctx context.Context, resource config.Resource) context.Context {
	return plugin.WithLogger(ctx, plugin.Logger(ctx).With("resource", resource.Kind+"/"+resource.Metadata.Name))
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
