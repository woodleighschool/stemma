package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/fileio"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Options configures one finite execution of the CLI.
type Options struct {
	ConfigPath, CacheDir, StateDir string
	Method                         string
	Resources                      []string
	Lock                           lockfile.Options
	Handlers                       map[string]reconcileHandler
}

// Report distinguishes source, preparation and each destination's work.
type Report struct {
	LockChanged *bool            `json:"lock_changed,omitempty"`
	Error       string           `json:"error,omitempty"`
	Resources   []ResourceReport `json:"resources"`
}

// ResourceReport separates immutable outputs from destination reconciliation.
type ResourceReport struct {
	Name           string              `json:"name"`
	Kind           string              `json:"kind"`
	Key            string              `json:"key"`
	InputCacheHits map[string]bool     `json:"input_cache_hits,omitempty"`
	Artifacts      map[string]Prepared `json:"artifacts,omitempty"`
	Cached         bool                `json:"cached"`
	Destinations   []DestinationReport `json:"destinations,omitempty"`
	Error          string              `json:"error,omitempty"`
}

// DestinationReport describes semantic drift independently of cache hits.
type DestinationReport struct {
	Name            string            `json:"name"`
	Origins         map[string]string `json:"origins,omitempty"`
	SourceChanged   bool              `json:"source_changed"`
	PreparedChanged bool              `json:"prepared_changed"`
	Changes         []plugin.Change   `json:"changes"`
	Applied         bool              `json:"applied"`
	Error           string            `json:"error,omitempty"`
}

type binding struct {
	Connection string          `json:"connection"`
	Source     string          `json:"source,omitempty"`
	Payload    string          `json:"payload,omitempty"`
	Binding    json.RawMessage `json:"binding"`
}
type state struct {
	Version  int                `json:"version"`
	Project  string             `json:"project"`
	Bindings map[string]binding `json:"bindings"`
}

// Run resolves locked resources in dependency order and reconciles destinations independently.
func Run(ctx context.Context, opts Options) (report Report, runErr error) {
	defer func() {
		if runErr != nil {
			report.Error = runErr.Error()
		}
	}()
	plugin.Stage(ctx, "Loading project")
	switch opts.Method {
	case "update", "prepare", "plan", "apply":
	default:
		return report, fmt.Errorf("unsupported run method %q", opts.Method)
	}
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return report, err
	}
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return report, err
	}
	unlock, err := lockfile.Lock(ctx, root)
	if err != nil {
		return report, err
	}
	defer func() { _ = unlock() }()
	store, err := cas.Open(opts.CacheDir)
	if err != nil {
		return report, err
	}
	release, err := store.Lease(ctx)
	if err != nil {
		return report, err
	}
	defer func() { _ = release() }()
	manager := source.New(store, root, opts.Lock.Offline)
	pluginWork, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "operations-*")
	if err != nil {
		return report, err
	}
	defer func() { _ = os.RemoveAll(pluginWork) }()
	if opts.Method == "plan" || opts.Method == "apply" {
		opts.Lock.Frozen = true
	}
	ops, err := loadOperations(ctx, p, manager, pluginWork, opts.Handlers, opts.Lock.Frozen || opts.Lock.Offline)
	if err != nil {
		return report, err
	}
	plugin.Stage(ctx, "Validating operation contracts")
	plans, err := discover(ctx, p, ops)
	if err != nil {
		return report, err
	}
	selected, err := orderResources(plans, opts.Resources)
	if err != nil {
		return report, err
	}
	if err := preflight(plans, selected, p, ops); err != nil {
		return report, err
	}
	if err := registerResolvers(manager, ops, pluginWork); err != nil {
		return report, err
	}
	destinations, dependencies, err := orderDestinations(ctx, p, plans, ops, root, selected)
	if err != nil {
		return report, err
	}
	plugin.Logger(ctx).InfoContext(ctx, "Resources selected", "count", len(selected))
	declarations := map[string]map[string]plugin.Input{}
	for _, key := range selected {
		declarations[key] = map[string]plugin.Input{}
		for name, input := range plans[key].Inputs {
			if input.Resource == nil {
				declarations[key][name] = input
			}
		}
	}
	opts.Lock.PreserveUnselected = len(opts.Resources) > 0
	locked, err := lockfile.Prepare(ctx, root, declarations, ops.plugins, manager, opts.Lock)
	if err != nil {
		return report, err
	}
	report.LockChanged = &locked.Changed
	if opts.Method == "update" {
		return report, nil
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(root, ".stemma", "state")
	}
	statePath := filepath.Join(stateDir, p.Project+".json")
	if opts.Method == "apply" {
		plugin.Stage(ctx, "Acquiring destination state lock")
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return report, err
		}
		lock := flock.New(filepath.Join(stateDir, p.Project+".lock"))
		ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
		if err != nil {
			return report, err
		}
		if !ok {
			return report, ctx.Err()
		}
		defer func() { _ = lock.Close() }()
	}
	current, err := loadState(statePath, p.Project)
	if err != nil {
		return report, err
	}
	type preparedResource struct {
		work    string
		outputs map[string]Prepared
		report  int
		ready   bool
	}
	preparedItems := map[string]preparedResource{}
	var failures []error
	for _, key := range selected {
		plan := plans[key]
		ctx := plugin.WithLogger(ctx, plugin.Logger(ctx).With("resource", plan.Resource.Kind+"/"+plan.Resource.Metadata.Name))
		plugin.Stage(ctx, "Preparing resource")
		started := time.Now()
		item := ResourceReport{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Key: key, InputCacheHits: locked.CacheHits[key]}
		work, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "resource-*")
		if err != nil {
			return report, err
		}
		defer func() { _ = os.RemoveAll(work) }()
		inputs := map[string]Prepared{}
		var preparationErr error
		for name, declaration := range plan.Inputs {
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
				entry, ok := locked.File.Inputs[key][name]
				if !ok {
					preparationErr = fmt.Errorf("missing locked input %s", name)
					break
				}
				inputs[name] = Prepared{Timestamp: entry.ResolvedAt, Payload: entry.Content.Artifact, Filename: entry.Content.Filename, Tree: entry.Content.Tree, Mode: entry.Content.Mode, InputsHash: entry.Content.Artifact.SHA256}
			}
		}
		var outputs map[string]Prepared
		if preparationErr == nil {
			outputs, item.Cached, preparationErr = prepareResource(ctx, store, ops, plan, inputs, work)
		}
		item.Artifacts = outputs
		if preparationErr != nil {
			item.Error = preparationErr.Error()
			failures = append(failures, fmt.Errorf("%s: %w", key, preparationErr))
		}
		if preparationErr == nil {
			plugin.Logger(ctx).InfoContext(ctx, "Resource prepared", "cached", item.Cached, "elapsed", time.Since(started).Round(time.Millisecond))
		} else {
			plugin.Logger(ctx).ErrorContext(ctx, "Preparation failed", "error", preparationErr)
		}
		preparedItems[key] = preparedResource{work, outputs, len(report.Resources), preparationErr == nil}
		report.Resources = append(report.Resources, item)
	}
	failed := map[destinationRef]bool{}
	for _, destination := range destinations {
		prepared := preparedItems[destination.Resource]
		item := &report.Resources[prepared.report]
		if !prepared.ready {
			failed[destination] = true
			continue
		}
		var destinationErr error
		for _, dependency := range dependencies[destination] {
			if failed[dependency] {
				destinationErr = fmt.Errorf("required publication %s/%s failed", dependency.Resource, dependency.Destination)
				break
			}
		}
		before := len(item.Destinations)
		if destinationErr == nil {
			destinationErr = reconcileDestination(ctx, opts, p, plans, ops, store, root, prepared.work, destination.Resource, destination.Destination, prepared.outputs, &current, statePath, item)
		}
		if destinationErr != nil {
			plugin.Logger(ctx).ErrorContext(ctx, "Destination failed", "resource", destination.Resource, "destination", destination.Destination, "error", destinationErr)
			failed[destination] = true
			if len(item.Destinations) == before {
				item.Destinations = append(item.Destinations, DestinationReport{Name: destination.Destination, Error: destinationErr.Error()})
			}
			if item.Error == "" {
				item.Error = destinationErr.Error()
			} else {
				item.Error += "\n" + destinationErr.Error()
			}
			failures = append(failures, fmt.Errorf("%s/%s: %w", destination.Resource, destination.Destination, destinationErr))
		}
	}
	return report, errors.Join(failures...)
}

type destinationInput struct {
	name     string
	prepared Prepared
	request  plugin.ReconcileRequest
	report   DestinationReport
}

func reconcileDestination(ctx context.Context, opts Options, p config.Project, plans map[string]resourcePlan, ops *operations, store *cas.Store, root, work, name, destination string, outputs map[string]Prepared, current *state, statePath string, item *ResourceReport) error {
	software := plans[name]
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("resource", software.Resource.Kind+"/"+software.Resource.Metadata.Name, "destination", destination))
	plugin.Stage(ctx, "Validating destination")
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
	if present && !prepared.SuppliedFacts && (operation.RequiresInspection || hasFactReference(metadata)) {
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
	input, err := makeDestinationInput(ops, p, plans, root, name, destination, prepared, effective, origins, current)
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
	if err := ops.call(ctx, d.Operation, "validate", input.request, nil); err != nil {
		return fmt.Errorf("destination %s: %w", destination, err)
	}
	if err := verifyLeases(ctx, store, work, input.request); err != nil {
		return err
	}
	if opts.Method == "prepare" {
		return nil
	}
	plugin.Stage(ctx, "Planning destination")
	input.request.Method = "plan"
	var response plugin.ReconcileResponse
	err = ops.call(ctx, d.Operation, "plan", input.request, &response)
	input.report.Changes = response.Changes
	input.report.Origins = mergeOrigins(input.report.Origins, response.Origins)
	err = errors.Join(err, verifyLeases(ctx, store, work, input.request))
	if err == nil {
		plugin.Logger(ctx).InfoContext(ctx, "Destination planned", "changes", len(response.Changes))
	}
	if err == nil && opts.Method == "apply" {
		input.report, err = deliver(ctx, ops, p, store, work, name, input, current, statePath)
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

func makeDestinationInput(ops *operations, p config.Project, plans map[string]resourcePlan, root, software, name string, prepared Prepared, metadata map[string]any, origins map[string]string, current *state) (destinationInput, error) {
	d := p.Destinations[name]
	previous := current.Bindings[software+"/"+name]
	if previous.Connection != ops.fingerprint(d) {
		previous = binding{Connection: ops.fingerprint(d)}
	}
	input := destinationInput{name: name, prepared: prepared, report: DestinationReport{Name: name, Origins: origins, SourceChanged: previous.Source != prepared.InputsHash, PreparedChanged: previous.Payload != prepared.Payload.SHA256}}
	metadataData, err := json.Marshal(metadata)
	if err != nil {
		return input, err
	}
	settings := d.Config
	configData, err := json.Marshal(settings)
	if err != nil {
		return input, err
	}
	input.request = plugin.ReconcileRequest{Method: "validate", Identity: plugin.Identity{Project: p.Project, Software: plans[software].Resource.Metadata.Name, Destination: name}, Config: configData, Metadata: metadataData, Artifact: prepared.artifact(), Facts: prepared.Facts, Binding: previous.Binding, Prepared: true, Root: root, Subjects: plans[software].Subjects, Bindings: peerBindings(ops, p, plans, name, current)}
	return input, nil
}

func deliver(ctx context.Context, ops *operations, p config.Project, store *cas.Store, work, software string, input destinationInput, current *state, statePath string) (DestinationReport, error) {
	d := p.Destinations[input.name]
	plugin.Stage(ctx, "Applying destination")
	report := input.report
	previous := current.Bindings[software+"/"+input.name]
	if previous.Connection != ops.fingerprint(d) {
		previous = binding{Connection: ops.fingerprint(d)}
	}
	if err := verifyLeases(ctx, store, work, input.request); err != nil {
		return report, err
	}
	input.request.Method = "apply"
	var response plugin.ReconcileResponse
	err := ops.call(ctx, d.Operation, "apply", input.request, &response)
	err = errors.Join(err, verifyLeases(ctx, store, work, input.request))
	report.Changes = response.Changes
	report.Origins = mergeOrigins(report.Origins, response.Origins)
	if err == nil || len(response.Binding) > 0 {
		if len(response.Binding) > 0 {
			previous.Binding = response.Binding
		}
		if err == nil {
			previous.Source = input.prepared.InputsHash
			previous.Payload = input.prepared.Payload.SHA256
			report.Applied = true
		}
		current.Bindings[software+"/"+input.name] = previous
		if saveErr := saveState(statePath, *current); saveErr != nil {
			return report, errors.Join(err, saveErr)
		}
	}
	if err == nil {
		plugin.Logger(ctx).InfoContext(ctx, "Destination applied", "changes", len(report.Changes))
	}
	return report, err
}

func verifyLeases(ctx context.Context, store *cas.Store, work string, request plugin.ReconcileRequest) error {
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

func hasFactReference(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if _, ok := value["$fact"]; ok {
			return true
		}
		for _, child := range value {
			if hasFactReference(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(value, hasFactReference)
	}
	return false
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

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func loadState(path, project string) (state, error) {
	s := state{Version: 2, Project: project, Bindings: map[string]binding{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if len(data) > 16<<20 {
		return s, errors.New("destination state exceeds 16 MiB")
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("destination state is corrupt; restore it before publication: %w", err)
	}
	if s.Version != 2 || s.Project != project || s.Bindings == nil {
		return s, errors.New("destination state has an unsupported version or different project identity")
	}
	return s, nil
}
func saveState(path string, s state) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fileio.Write(path, append(data, '\n'), 0o600)
}
