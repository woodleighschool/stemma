package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/oauth2"
)

// Options configures one finite execution of the CLI.
type Options struct {
	ConfigPath, CacheDir string
	// GitHubToken authenticates release lookups of GitHub sources that declare
	// no token.
	GitHubToken string
	// GitHubAppToken supplies reconcile's github.com App credential as the
	// release discovery fallback, renewing it at lookup time.
	GitHubAppToken oauth2.TokenSource
	Method         string
	Resources      []string
	// Profiles selects the resources declaring any of these profiles, in place
	// of the resources that declare none. A run names profiles or Resources.
	Profiles []string
	// ChangedSince narrows prepare to the resources whose preparation differs
	// from the catalog at this Git revision, after checking the lock's
	// shape.
	ChangedSince string
	// Jobs bounds independent preparations; zero uses two workers.
	Jobs  int
	Lock  lock.Options
	Icons IconOptions
	// Input selects artwork or facts without building the selected resource.
	Input InputSelection
	// Output names the resource output the artifact method materializes;
	// empty selects installer.
	Output string
	// OutputFile exports the selected artifact outside the disposable cache.
	OutputFile string
	// Read names a file inside the selected artifact or input. The artifact
	// and inspect methods write it to ReadTo in place of their own result.
	Read     string
	ReadTo   io.Writer
	Handlers map[string]reconcileHandler
	// Started receives what the run will report on once it has selected its
	// resources, before anything is acquired.
	Started func(Selection)
	// ResourceDone receives each final resource result, including failures.
	ResourceDone func(ResourceReport) error
}

// Selection identifies resources with multiple inputs, as Kind/name, so a
// report names the changed input even when only one of them changed.
type Selection struct {
	NamedInputs []string
}

// Run resolves locked resources in dependency order and reconciles destinations independently.
func Run(ctx context.Context, opts Options) (report Report, runErr error) {
	selected := 0
	defer func() {
		report.Command = opts.Method
		// Publication can stop between destinations. Keep the observed results
		// even when the resource did not reach its normal completion callback.
		if opts.Method == "plan" || opts.Method == "apply" {
			for i := range report.Resources {
				resource := &report.Resources[i]
				if resource.Status != "" || !slices.ContainsFunc(resource.Publications, func(p Publication) bool { return p.Status != "" }) {
					continue
				}
				resource.Status = StatusFailed
				resource.Error = cmp.Or(resource.Error, "run stopped before all destinations completed")
				if opts.ResourceDone != nil {
					runErr = errors.Join(runErr, opts.ResourceDone(*resource))
				}
			}
		}
		// Prepared resources that never reached a destination have no outcome.
		report.Resources = slices.DeleteFunc(report.Resources, func(resource ResourceReport) bool { return resource.Status == "" })
		report.Summarize(selected)
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
	if opts.Jobs < 0 {
		return report, errors.New("jobs must not be negative")
	}
	opts.Jobs = cmp.Or(opts.Jobs, 2)
	if opts.Read != "" {
		switch {
		case opts.Method != "artifact" && opts.Method != "inspect":
			return report, fmt.Errorf("%s reads no file inside an artifact", opts.Method)
		case opts.ReadTo == nil:
			return report, errors.New("read requires a writer")
		case opts.OutputFile != "" || opts.Input.Path != "":
			return report, errors.New("read replaces the artifact export and the subject path")
		}
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
		if opts.Icons.From != "" {
			// A local source stops short of the session: nothing is locked or acquired.
			err := iconFrom(ctx, opts, &report)
			return report, err
		}
	}
	// Update writes the lock and resolves plugin declarations. Every other
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
	if err := e.begin(ctx); err != nil {
		return report, err
	}
	selected = len(e.selected)
	if opts.Started != nil {
		opts.Started(e.selection())
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
	locked       *lock.Update
	inputOwner   string

	prepared map[string]preparedResource
	// pending counts each resource's destinations still to reconcile; the
	// resource completes when none remain.
	pending            map[string]int
	reconciled, failed map[destinationRef]bool
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
// opens the lock, before anything is acquired.
func (e *execution) begin(ctx context.Context) (err error) {
	s := e.session
	done := plugin.Stage(ctx, "Checking resource configuration")
	defer func() { done(err) }()
	e.roots, err = selectResources(s.project.Resources, e.opts.Resources, e.opts.Profiles)
	if err != nil {
		return err
	}
	if err := registerResolvers(s.manager, s.ops, s.work); err != nil {
		return err
	}
	if e.opts.ChangedSince != "" {
		if e.roots, err = changedSince(ctx, s, e.opts.ChangedSince, e.roots, e.opts.Profiles); err != nil {
			return err
		}
	}
	// Updates, artifacts and icons never reach a destination, so only the
	// other runs check the destinations they publish to.
	usesDestinations := e.opts.Method != "update" && e.opts.Method != "artifact" && e.opts.Method != "icon" && e.opts.Method != "inspect"
	derive := ""
	if e.opts.Method == "signature" {
		derive = "signature"
	}
	e.plans, e.selected, err = discoverClosure(ctx, s.project, s.ops, e.roots, true, usesDestinations, derive)
	if err != nil {
		return err
	}
	if err := preflight(e.plans, e.selected, s.project, s.ops, usesDestinations); err != nil {
		return err
	}
	if e.opts.Method == "inspect" && e.opts.Input.Name == "" {
		return errors.New("inspect requires an input name")
	}
	// Without an input name, a path selects within one prepared installer.
	if e.opts.Input.Path != "" && e.opts.Input.Name == "" && len(e.roots) != 1 {
		return errors.New("path requires one resource selector")
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
	opts.PreserveUnselected = len(e.opts.Resources) > 0 || len(e.opts.Profiles) > 0 || e.opts.ChangedSince != ""
	opts.Retain = unselected(s.project.Resources, e.selected)
	plugin.Logger(ctx).DebugContext(ctx, "Resources selected", "count", len(e.selected), "unselected", len(opts.Retain))
	e.locked, err = lock.Begin(ctx, s.root, inputs, s.ops.plugins, s.manager, opts)
	return err
}

// run prepares selected resources and reconciles their destinations,
// then finishes the project lock update.
func (e *execution) run(ctx context.Context) error {
	if e.opts.Jobs > 1 && e.opts.Method != "update" {
		if err := e.prepareAll(ctx); err != nil {
			return err
		}
	}
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
	if e.opts.Method == "update" {
		if e.report.LockChanged == nil {
			e.report.LockChanged = new(bool)
		}
		*e.report.LockChanged = *e.report.LockChanged || result.Changed
		e.report.Plugins = result.Plugins
	}
	reported := map[string]bool{}
	for _, resource := range e.report.Resources {
		reported[resource.Key] = true
	}
	for _, change := range result.Changes {
		if !reported[change.Resource] {
			e.report.Removed = append(e.report.Removed, change)
		}
	}
	return err
}

func (e *execution) selection() Selection {
	var selection Selection
	for _, key := range e.selected {
		plan := e.plans[key]
		if e.opts.Method == "update" && len(plan.Inputs) > 1 {
			selection.NamedInputs = append(selection.NamedInputs, plan.Resource.Kind+"/"+plan.Resource.Metadata.Name)
		}
	}
	return selection
}

// icons creates each selected resource's missing or forced icon. Only a
// missing or forced asset costs a preparation, so a run across the catalog is
// cheap; creating the icon completes the resource in place of publication.
func (e *execution) icons(ctx context.Context) error {
	if e.opts.Input.Name != "" {
		return e.inputIcon(ctx)
	}
	outcomes, building := map[string]*ResourceReport{}, map[string]bool{}
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
		if outcomes[key] = iconOutcome(e.opts.Icons, e.session.root, e.plans[key]); outcomes[key] == nil {
			e.pending[key] = 1
			builds(key)
		}
	}
	for _, key := range e.selected {
		if outcome := outcomes[key]; outcome != nil {
			if building[key] {
				continue // Preparing the resource that consumes it reports it.
			}
			resource := e.plans[key].Resource
			outcome.Name, outcome.Kind, outcome.Key = resource.Metadata.Name, resource.Kind, key
			e.report.Resources = append(e.report.Resources, *outcome)
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
		// A path names an application in the selected resource's installer,
		// not in the builds it consumes.
		var selection InputSelection
		if key == e.roots[0] {
			selection.Path = e.opts.Input.Path
		}
		installer := prepared.outputs["installer"]
		if installer.Payload.SHA256 != "" {
			var leased plugin.Artifact
			leased, err = lend(ctx, e.session.store, installer, filepath.Join(prepared.work, "artwork"), false)
			installer.Path = leased.Path
		}
		if err == nil {
			err = createIcon(resourceContext(ctx, e.plans[key].Resource), e.opts.Icons, e.session.root, e.plans[key], installer, selection, prepared.work, item)
		}
		if err != nil {
			item.Error = err.Error()
			e.fail(ctx, ResourceError{Resource: key, Err: err})
		}
		cleanupError(ctx, os.RemoveAll(prepared.work))
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
// the reviewed lock and exposes the selected output. No destination sees
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
	defer func() { cleanupError(ctx, os.RemoveAll(prepared.work)) }()
	item := &e.report.Resources[prepared.report]
	output := cmp.Or(e.opts.Output, "installer")
	artifact, ok := prepared.outputs[output]
	var err error
	switch {
	case !ok:
		err = fmt.Errorf("no %s output; the resource prepares %s", output, strings.Join(sortedKeys(prepared.outputs), ", "))
	case e.opts.Read != "":
		err = e.readStored(resourceContext(ctx, e.plans[key].Resource), artifact, filepath.Join(prepared.work, "read"))
	default:
		e.report.Artifact, err = expose(resourceContext(ctx, e.plans[key].Resource), e.session.store, artifact, filepath.Join(prepared.work, "materialized"), e.opts.OutputFile)
	}
	if err != nil {
		item.Error = err.Error()
		e.fail(ctx, ResourceError{Resource: key, Err: err})
	}
	return e.complete(ctx, item)
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
		evidence := maps.Clone(entry.Evidence)
		if entry.Version != "" {
			if evidence == nil {
				evidence = map[string]json.RawMessage{}
			}
			evidence["source"], _ = json.Marshal(map[string]string{"version": entry.Version})
		}
		return Prepared{Payload: ref, Filename: entry.Content.Filename, Tree: entry.Content.Tree, Mode: entry.Content.Mode, Version: entry.Version, ContentRoot: entry.Content.Root, InputsHash: entry.Content.SHA256, Evidence: evidence}, nil
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

// complete saves an update's acquired inputs before reporting its final result.
func (e *execution) complete(ctx context.Context, item *ResourceReport) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var saveErr error
	if item.Error == "" {
		if e.opts.Method == "update" {
			result, err := e.locked.CommitResource(ctx, item.Key)
			if e.report.LockChanged == nil {
				e.report.LockChanged = new(bool)
			}
			*e.report.LockChanged = *e.report.LockChanged || result.Changed
			e.report.Plugins = result.Plugins
			item.Inputs = result.Changes
			if err != nil {
				saveErr = fmt.Errorf("save reviewed inputs: %w", err)
				item.Error = saveErr.Error()
			}
		} else {
			item.Inputs = e.locked.Changes(item.Key)
		}
	}
	plan := e.plans[item.Key]
	item.status(e.opts.Method)
	item.Notices = signatureNotices(plan.Resource.Reference(), item.Artifacts)
	if e.prepared[item.Key].ready {
		item.Notices = slices.Concat(plan.Notices, item.Notices)
	}
	if e.opts.ResourceDone != nil {
		if err := e.opts.ResourceDone(*item); err != nil {
			return errors.Join(saveErr, err)
		}
	}
	return errors.Join(saveErr, ctx.Err())
}

// fail records a resource failure, unless the run was interrupted and the
// failure is only its echo.
func (e *execution) fail(ctx context.Context, err ResourceError) {
	if ctx.Err() == nil {
		e.failures = append(e.failures, err)
	}
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
