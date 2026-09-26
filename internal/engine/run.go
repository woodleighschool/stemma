package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/expression"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
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
	// Output names the resource output the artifact method materializes;
	// empty selects installer.
	Output   string
	Handlers map[string]reconcileHandler
	// ResourceDone receives each final resource result, including failures.
	ResourceDone func(ResourceReport) error
}

// ResourceError identifies a resource failure independently of command-wide failures.
type ResourceError struct {
	Resource string
	Err      error
}

func (e ResourceError) Error() string { return e.Resource + ": " + e.Err.Error() }
func (e ResourceError) Unwrap() error { return e.Err }

// Unreported is the part of a run's error that its report does not carry with
// a resource, or nil. Joined errors are split; a wrapped error keeps its
// context whole.
func Unreported(err error) error {
	if _, ok := err.(ResourceError); ok { //nolint:errorlint // A wrapped resource failure has context the report lacks.
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var rest []error
	for _, child := range joined.Unwrap() {
		if child := Unreported(child); child != nil {
			rest = append(rest, child)
		}
	}
	return errors.Join(rest...)
}

// Report distinguishes source, preparation and each destination's work.
type Report struct {
	// LockChanged reports whether update wrote the lockfile.
	LockChanged *bool `json:"lock_changed,omitempty"`
	// RemovedInputs are lock entries of resources the catalog no longer declares.
	RemovedInputs []lockfile.InputChange `json:"removed_inputs,omitempty"`
	Warnings      []string               `json:"warnings,omitempty"`
	Summary       Summary                `json:"summary"`
	Error         string                 `json:"error,omitempty"`
	Resources     []ResourceReport       `json:"resources"`
	// Artifact is where the artifact method materialized the selected output.
	Artifact string `json:"artifact,omitempty"`
}

// ResourceReport separates immutable outputs from destination reconciliation.
type ResourceReport struct {
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Key            string          `json:"key"`
	InputCacheHits map[string]bool `json:"input_cache_hits,omitempty"`
	// Inputs are the lock changes this run commits for the resource.
	Inputs       []lockfile.InputChange `json:"inputs,omitempty"`
	Artifacts    map[string]Prepared    `json:"artifacts,omitempty"`
	Cached       bool                   `json:"cached"`
	Destinations []DestinationReport    `json:"destinations,omitempty"`
	// Icon reports what the icon method did for the resource's declared asset.
	Icon  string `json:"icon,omitempty"`
	Error string `json:"error,omitempty"`
	// BlockedBy names the resources whose unavailable outputs prevented execution.
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// DestinationReport describes semantic drift independently of cache hits.
type DestinationReport struct {
	Name    string            `json:"name"`
	Origins map[string]string `json:"origins,omitempty"`
	Changes []plugin.Change   `json:"changes"`
	Applied bool              `json:"applied"`
	Error   string            `json:"error,omitempty"`
}

type preparedResource struct {
	work    string
	outputs map[string]Prepared
	report  int
	ready   bool
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
	case "artifact":
		if len(opts.Resources) != 1 {
			return report, errors.New("artifact requires one resource selector")
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
	// signature derives each resource's signer through the preparation path;
	// icon presents the artwork of prepared software the same way.
	preparing := opts.Method == "prepare" || opts.Method == "signature" || opts.Method == "icon"
	// Update writes the lockfile. Every other run consumes it as reviewed, with
	// the plugins it pins.
	opts.Lock.Refresh = opts.Method == "update"
	s, err := open(ctx, opts, !opts.Lock.Refresh)
	if err != nil {
		return report, err
	}
	defer s.close()
	p, root, store, manager, ops := s.project, s.root, s.store, s.manager, s.ops
	done := plugin.Stage(ctx, "Validating operation contracts")
	defer func() { done(runErr) }()
	roots, err := selectResources(p.Resources, opts.Resources)
	if err != nil {
		return report, err
	}
	if opts.ChangedSince != "" {
		if roots, err = changedSince(ctx, s, opts.ChangedSince); err != nil {
			return report, err
		}
	}
	plans, selected, err := discoverClosure(ctx, p, ops, roots, true)
	if err != nil {
		return report, err
	}
	if err := preflight(plans, selected, p, ops); err != nil {
		return report, err
	}
	if opts.Method == "plan" || opts.Method == "apply" {
		// Locking and icon creation come first for a new declaration, so only
		// publication requires the asset.
		if err := verifyIcons(root, plans, selected); err != nil {
			return report, err
		}
	}
	if err := registerResolvers(manager, ops, s.work); err != nil {
		return report, err
	}
	publishing := opts.Method == "plan" || opts.Method == "apply"
	destinations, err := planDestinations(ctx, p, plans, ops, root, selected, publishing)
	if err != nil {
		return report, err
	}
	// Publication connects with evaluated settings, so a missing value fails
	// before anything is acquired.
	connections := map[string]json.RawMessage{}
	if publishing {
		for node := range destinations {
			connections[node.Destination] = nil
		}
		for _, name := range sortedKeys(connections) {
			connections[name], err = connectionSettings(ops, name, p.Destinations[name])
			if err != nil {
				return report, err
			}
		}
	}
	done(nil)
	declarations := declarations(plans, selected)
	opts.Lock.PreserveUnselected = len(opts.Resources) > 0 || opts.ChangedSince != ""
	opts.Lock.Retain = suspended(p.Resources)
	plugin.Logger(ctx).DebugContext(ctx, "Resources selected", "count", len(selected), "suspended", len(opts.Lock.Retain))
	locked, err := lockfile.Begin(ctx, root, declarations, ops.plugins, manager, opts.Lock)
	if err != nil {
		return report, err
	}
	report.Resources = []ResourceReport{}
	preparedItems := map[string]preparedResource{}
	pending := map[string]int{}
	if opts.Method != "update" {
		for destination := range destinations {
			pending[destination.Resource]++
		}
	}
	// complete reports a finished resource. A rejected resource keeps its
	// reviewed entries, so only a successful one has input changes.
	complete := func(item *ResourceReport) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.Error == "" {
			item.Inputs = locked.Changes(item.Key)
		}
		if opts.ResourceDone != nil {
			if err := opts.ResourceDone(*item); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	var workdirs []string
	defer func() {
		if len(workdirs) == 0 {
			return
		}
		cleanupDone := plugin.Stage(ctx, "Removing workspaces")
		var cleanupErr error
		for _, work := range workdirs {
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(work))
		}
		cleanupDone(cleanupErr)
	}()
	var failures []error
	var prepare func(string) error
	prepare = func(key string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := preparedItems[key]; exists {
			return nil
		}
		plan := plans[key]
		for _, name := range sortedKeys(plan.Inputs) {
			if ref := plan.Inputs[name].Resource; ref != nil {
				if err := prepare(ref.Key()); err != nil {
					return err
				}
			}
		}
		ctx := resourceContext(ctx, plan.Resource)
		plugin.Logger(ctx).DebugContext(ctx, "Executing resource")
		started := time.Now()
		item := ResourceReport{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Key: key}
		for _, producer := range producers(plan) {
			if !preparedItems[producer].ready {
				item.BlockedBy = append(item.BlockedBy, producer)
			}
		}
		var preparationErr error
		var entries map[string]source.Entry
		if len(item.BlockedBy) > 0 {
			preparationErr = fmt.Errorf("blocked by %s", strings.Join(item.BlockedBy, ", "))
		} else {
			entries, item.InputCacheHits, preparationErr = locked.Acquire(ctx, key)
		}
		var work string
		if preparationErr == nil && opts.Method != "update" {
			var err error
			work, err = os.MkdirTemp(filepath.Join(store.Dir, "work"), "resource-*")
			if err != nil {
				return err
			}
			workdirs = append(workdirs, work)
		}
		inputs := map[string]Prepared{}
		if preparationErr == nil && opts.Method != "update" {
			for _, name := range sortedKeys(plan.Inputs) {
				declaration := plan.Inputs[name]
				if ref := declaration.Resource; ref != nil {
					producer := preparedItems[ref.Key()]
					output := ref.Output
					if output == "" {
						output = "installer"
					}
					artifact, ok := producer.outputs[output]
					if !producer.ready || !ok {
						preparationErr = fmt.Errorf("input %s requires successful %s output %s", name, ref.Key(), output)
						break
					}
					inputs[name] = artifact
				} else {
					entry := entries[name]
					inputs[name] = Prepared{Payload: entry.Content.Artifact, Filename: entry.Content.Filename, Tree: entry.Content.Tree, Mode: entry.Content.Mode, InputsHash: entry.Content.Artifact.SHA256, Evidence: entry.Evidence}
				}
			}
		}
		var outputs map[string]Prepared
		if preparationErr == nil && opts.Method != "update" {
			derive := ""
			if opts.Method == "signature" {
				derive = "signature"
			}
			outputs, item.Cached, preparationErr = prepareResource(ctx, store, ops, plan, inputs, work, derive)
		}
		item.Artifacts = outputs
		if preparationErr != nil {
			item.Error = preparationErr.Error()
			if len(item.BlockedBy) == 0 && ctx.Err() == nil {
				failures = append(failures, ResourceError{Resource: key, Err: preparationErr})
			}
		}
		preparedItems[key] = preparedResource{work, outputs, len(report.Resources), preparationErr == nil}
		report.Resources = append(report.Resources, item)
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case preparationErr == nil && opts.Method == "update":
			plugin.Logger(ctx).DebugContext(ctx, "Inputs resolved", "elapsed", time.Since(started).Round(time.Millisecond))
		case preparationErr == nil:
			artifact := outputs["installer"]
			plugin.Logger(ctx).DebugContext(ctx, "Prepared", "artifact", artifact.Filename, "version", artifact.Version, "cached", item.Cached, "elapsed", time.Since(started).Round(time.Millisecond))
		case len(item.BlockedBy) > 0:
			plugin.Logger(ctx).DebugContext(ctx, "Resource blocked", "blocked_by", item.BlockedBy)
		default:
			plugin.Logger(ctx).DebugContext(ctx, "Preparation failed", "error", preparationErr)
		}
		if preparationErr != nil || pending[key] == 0 {
			return complete(&report.Resources[preparedItems[key].report])
		}
		return nil
	}
	failed, reconciled := map[destinationRef]bool{}, map[destinationRef]bool{}
	var reconcile func(destinationRef) error
	reconcile = func(destination destinationRef) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if reconciled[destination] {
			return nil
		}
		for _, dependency := range destinations[destination].requires {
			if _, selected := declarations[dependency.Resource]; selected {
				if err := reconcile(dependency); err != nil {
					return err
				}
			}
		}
		if err := prepare(destination.Resource); err != nil {
			return err
		}
		prepared := preparedItems[destination.Resource]
		item := &report.Resources[prepared.report]
		reconciled[destination] = true
		if !prepared.ready {
			failed[destination] = true
			return nil
		}
		var destinationErr error
		if !preparing {
			for _, dependency := range destinations[destination].requires {
				if failed[dependency] {
					destinationErr = fmt.Errorf("required publication %s/%s failed", dependency.Resource, dependency.Destination)
					break
				}
			}
		}
		before := len(item.Destinations)
		// Preparation checks metadata against the prepared artifact, except
		// metadata that reads env values, which only publication evaluates.
		if destinationErr == nil && (publishing || !destinations[destination].environment) {
			operation, err := ops.operation(p.Destinations[destination.Destination].Operation)
			if err != nil {
				return err
			}
			peers, err := resolvePeerMetadata(ctx, plans, destination.Destination, destinations[destination].peers, preparedItems, operation.MetadataSchema)
			destinationErr = err
			if destinationErr == nil {
				destinationErr = reconcileDestination(ctx, opts, p, plans, ops, store, root, prepared.work, destination.Resource, destination.Destination, connections[destination.Destination], prepared.outputs, peers, item)
			}
		}
		if destinationErr != nil {
			failed[destination] = true
			if len(item.Destinations) == before {
				item.Destinations = append(item.Destinations, DestinationReport{Name: destination.Destination, Error: destinationErr.Error()})
			}
			if item.Error == "" {
				item.Error = destinationErr.Error()
			} else {
				item.Error += "\n" + destinationErr.Error()
			}
			if ctx.Err() == nil {
				failures = append(failures, ResourceError{Resource: destination.Resource, Err: fmt.Errorf("%s: %w", destination.Destination, destinationErr)})
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if destinationErr != nil {
			plugin.Logger(ctx).DebugContext(ctx, "Destination failed", "resource", item.Kind+"/"+item.Name, "destination", destination.Destination, "error", destinationErr)
		}
		pending[destination.Resource]--
		if pending[destination.Resource] == 0 {
			return complete(item)
		}
		return nil
	}
	if opts.Method == "icon" {
		// Only a missing or forced asset costs a preparation, so a run across the
		// catalog is cheap; creating the icon then completes the resource in place of
		// publication.
		clear(pending)
		outcomes, building := map[string]string{}, map[string]bool{}
		var builds func(string)
		builds = func(key string) {
			for _, input := range plans[key].Inputs {
				if ref := input.Resource; ref != nil && !building[ref.Key()] {
					building[ref.Key()] = true
					builds(ref.Key())
				}
			}
		}
		for _, key := range selected {
			if outcomes[key] = iconOutcome(opts.Icons, root, plans[key]); outcomes[key] == "" {
				pending[key] = 1
				builds(key)
			}
		}
		for _, key := range selected {
			if outcome := outcomes[key]; outcome != "" {
				if building[key] {
					continue // Preparing the resource that consumes it reports it.
				}
				resource := plans[key].Resource
				item := ResourceReport{Name: resource.Metadata.Name, Kind: resource.Kind, Key: key, Icon: outcome}
				report.Resources = append(report.Resources, item)
				if err := complete(&report.Resources[len(report.Resources)-1]); err != nil {
					return report, errors.Join(append(failures, err)...)
				}
				continue
			}
			if err := prepare(key); err != nil {
				return report, errors.Join(append(failures, err)...)
			}
			prepared := preparedItems[key]
			if !prepared.ready {
				continue
			}
			item := &report.Resources[prepared.report]
			item.Icon, err = createIcon(resourceContext(ctx, plans[key].Resource), opts.Icons, root, plans[key], prepared.outputs, prepared.work)
			if err != nil {
				item.Error = err.Error()
				if ctx.Err() == nil {
					failures = append(failures, ResourceError{Resource: key, Err: err})
				}
			}
			if err := ctx.Err(); err != nil {
				return report, errors.Join(append(failures, err)...)
			}
			if err := complete(item); err != nil {
				return report, errors.Join(append(failures, err)...)
			}
		}
		return report, errors.Join(failures...)
	}
	if opts.Method == "artifact" {
		// The selected resource and the builds it consumes are prepared from the
		// reviewed lockfile. No destination sees the prepared artifact.
		key := roots[0]
		clear(pending)
		pending[key] = 1
		if err := prepare(key); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		prepared := preparedItems[key]
		if !prepared.ready {
			return report, errors.Join(failures...)
		}
		item := &report.Resources[prepared.report]
		output := cmp.Or(opts.Output, "installer")
		artifact, ok := prepared.outputs[output]
		if ok {
			report.Artifact, err = expose(resourceContext(ctx, plans[key].Resource), store, artifact, filepath.Join(prepared.work, "materialized"))
		} else {
			err = fmt.Errorf("no %s output; the resource prepares %s", output, strings.Join(sortedKeys(prepared.outputs), ", "))
		}
		if err != nil {
			item.Error = err.Error()
			if ctx.Err() == nil {
				failures = append(failures, ResourceError{Resource: key, Err: err})
			}
		}
		if err := complete(item); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		return report, errors.Join(failures...)
	}
	for _, key := range selected {
		if opts.Method == "update" || len(plans[key].Destinations) == 0 {
			if err := prepare(key); err != nil {
				return report, errors.Join(append(failures, err)...)
			}
		}
		if opts.Method == "update" {
			continue
		}
		for _, destination := range sortedKeys(plans[key].Destinations) {
			if err := reconcile(destinationRef{key, destination}); err != nil {
				return report, errors.Join(append(failures, err)...)
			}
		}
	}
	var rejected []string
	for _, resource := range report.Resources {
		if resource.Error != "" {
			rejected = append(rejected, resource.Key)
		}
	}
	result, err := locked.Commit(ctx, rejected...)
	if err != nil {
		return report, errors.Join(append(failures, err)...)
	}
	if opts.Method == "update" {
		report.LockChanged = &result.Changed
	}
	reported := map[string]bool{}
	for _, resource := range report.Resources {
		reported[resource.Key] = true
	}
	for _, change := range result.Changes {
		if !reported[change.Resource] {
			report.RemovedInputs = append(report.RemovedInputs, change)
		}
	}
	return report, errors.Join(failures...)
}

type destinationInput struct {
	name    string
	request plugin.ReconcileRequest[json.RawMessage]
	report  DestinationReport
}

func reconcileDestination(ctx context.Context, opts Options, p config.Project, plans map[string]resourcePlan, ops *operations, store *cas.Store, root, work, name, destination string, settings json.RawMessage, outputs map[string]Prepared, peers map[string]json.RawMessage, item *ResourceReport) (runErr error) {
	software := plans[name]
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("resource", software.Resource.Kind+"/"+software.Resource.Metadata.Name, "destination", destination))
	done := plugin.Stage(ctx, "Validating destination")
	defer func() { done(runErr) }()
	prepared, present := outputs["installer"]
	if reference, ok := software.Destinations[destination]["installer"].(string); ok {
		prepared, present = outputs[reference]
		if !present {
			return fmt.Errorf("destination %s references missing output %s", destination, reference)
		}
	}
	d := p.Destinations[destination]
	metadata := destinationMetadata(software.Destinations[destination])
	operation, err := ops.operation(d.Operation)
	if err != nil {
		return err
	}
	if operation.Content != nil {
		if err := operation.Content.Accepts(prepared.artifact()); err != nil {
			return fmt.Errorf("destination %s: %w", destination, err)
		}
	}
	roots, err := expression.Roots(metadata)
	if err != nil {
		return err
	}
	if present && !prepared.SuppliedFacts && (operation.RequiresInspection || slices.Contains(roots, "facts")) {
		prepared.Facts, err = inspection.Read(ctx, prepared.Path)
		if err != nil {
			return fmt.Errorf("destination %s required inspection: %w", destination, err)
		}
	}
	effective, origins, err := resolveMetadata(software.ResourceResult, metadata, prepared.Facts, prepared.Evidence)
	if err != nil {
		return fmt.Errorf("destination %s metadata: %w", destination, err)
	}
	if present {
		prepared, err = materialize(ctx, store, prepared, filepath.Join(work, "destinations", destination))
		if err != nil {
			return err
		}
	}
	input, err := makeDestinationInput(p, plans, root, name, destination, prepared, effective, origins, peers)
	if err != nil {
		return err
	}
	input.request.Inputs = map[string]plugin.Artifact{}
	references := map[string]string{}
	for output := range outputs {
		if output != "installer" {
			references[output] = output
		}
	}
	maps.Copy(references, destinationReferences(software.Destinations[destination]))
	for inputName, reference := range references {
		artifact, exists := outputs[reference]
		if !exists {
			return fmt.Errorf("destination %s missing input %s output %s", destination, inputName, reference)
		}
		artifact, err = materialize(ctx, store, artifact, filepath.Join(work, "destinations", destination, "inputs", inputName))
		if err != nil {
			return err
		}
		input.request.Inputs[inputName] = artifact.artifact()
	}
	if software.Icon != "" && (opts.Method == "plan" || opts.Method == "apply") {
		artifact, err := iconInput(root, software.Icon, filepath.Join(work, "destinations", destination, "inputs", "icon"))
		if err != nil {
			return fmt.Errorf("destination %s: %w", destination, err)
		}
		input.request.Inputs["icon"] = artifact
	}
	if err := ops.call(ctx, d.Operation, "validate", input.request, nil); err != nil {
		return fmt.Errorf("destination %s: %w", destination, err)
	}
	if err := verifyLeases(ctx, store, work, input.request); err != nil {
		return err
	}
	if opts.Method == "prepare" || opts.Method == "signature" {
		return nil
	}
	done(nil)
	done = plugin.Stage(ctx, "Planning destination")
	input.request.Method = "plan"
	input.request.Config = settings
	var response plugin.ReconcileResponse
	err = ops.call(ctx, d.Operation, "plan", input.request, &response)
	input.report.Changes = response.Changes
	input.report.Origins = mergeOrigins(input.report.Origins, response.Origins)
	err = errors.Join(err, verifyLeases(ctx, store, work, input.request))
	done(err, plugin.Detail(changeCount(len(response.Changes))))
	if err == nil && opts.Method == "apply" {
		input.report, err = deliver(ctx, ops, p, store, work, input)
	}
	if err != nil {
		input.report.Error = err.Error()
	}
	item.Destinations = append(item.Destinations, input.report)
	if err != nil {
		return fmt.Errorf("destination %s: %w", destination, err)
	}
	return nil
}

func makeDestinationInput(p config.Project, plans map[string]resourcePlan, root, software, name string, prepared Prepared, metadata map[string]any, origins map[string]string, peers map[string]json.RawMessage) (destinationInput, error) {
	input := destinationInput{name: name, report: DestinationReport{Name: name, Origins: origins}}
	metadataData, err := json.Marshal(metadata)
	if err != nil {
		return input, err
	}
	minimum, err := minimumOS(prepared.artifact(), plans[software].MinimumOS)
	if err != nil {
		return input, err
	}
	input.request = plugin.ReconcileRequest[json.RawMessage]{Method: "validate", Identity: plugin.Identity{Project: p.Project, Resource: plans[software].Resource.Reference(), Destination: name}, Metadata: metadataData, Artifact: prepared.artifact(), MinimumOS: minimum, Prepared: true, Root: root, Peers: peers}
	return input, nil
}

// connectionSettings evaluates a destination's settings for publication and
// checks them against its operation's schema.
func connectionSettings(ops *operations, name string, destination config.Destination) (json.RawMessage, error) {
	settings, err := destination.ResolvedConfig()
	if err == nil {
		err = ops.configuration(destination.Operation, settings, true)
	}
	if err != nil {
		return nil, fmt.Errorf("destination %s: %w", name, err)
	}
	return json.Marshal(settings)
}

func deliver(ctx context.Context, ops *operations, p config.Project, store *cas.Store, work string, input destinationInput) (result DestinationReport, runErr error) {
	done := plugin.Stage(ctx, "Applying destination")
	defer func() { done(runErr) }()
	report := input.report
	if err := verifyLeases(ctx, store, work, input.request); err != nil {
		return report, err
	}
	input.request.Method = "apply"
	var response plugin.ReconcileResponse
	err := ops.call(ctx, p.Destinations[input.name].Operation, "apply", input.request, &response)
	err = errors.Join(err, verifyLeases(ctx, store, work, input.request))
	report.Changes = response.Changes
	report.Origins = mergeOrigins(report.Origins, response.Origins)
	report.Applied = err == nil
	done(err, plugin.Detail(changeCount(len(report.Changes))))
	return report, err
}

// changeCount describes a destination stage's changes for progress displays.
func changeCount(count int) string {
	switch count {
	case 0:
		return "no changes"
	case 1:
		return "1 change"
	}
	return strconv.Itoa(count) + " changes"
}

func verifyLeases(ctx context.Context, store *cas.Store, work string, request plugin.ReconcileRequest[json.RawMessage]) error {
	var artifacts []plugin.Artifact
	if request.Artifact.Path != "" {
		artifacts = append(artifacts, request.Artifact)
	}
	for _, input := range request.Inputs {
		artifacts = append(artifacts, input)
	}
	for _, artifact := range artifacts {
		ref, err := importPath(ctx, store, artifact.Path, artifact.Tree, work)
		if err != nil {
			return err
		}
		info, err := os.Stat(artifact.Path)
		if err != nil {
			return err
		}
		if ref.SHA256 != artifact.SHA256 || ref.Size != artifact.Size || uint32(info.Mode().Perm()) != artifact.Mode {
			return errors.New("operation modified an immutable leased artifact")
		}
	}
	return nil
}

// Validate checks the generic resource envelope without acquiring inputs.
func Validate(ctx context.Context, p config.Project) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Validate()
}

func destinationReferences(metadata map[string]any) map[string]string {
	result := map[string]string{}
	inputs, _ := metadata["inputs"].(map[string]any)
	for name, value := range inputs {
		if reference, ok := value.(string); ok {
			result[name] = reference
		}
	}
	return result
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
