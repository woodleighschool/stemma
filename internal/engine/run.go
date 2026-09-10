package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/woodleighschool/stemma/internal/munkirepo"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Options configures one finite execution of the CLI.
type Options struct {
	ConfigPath, CacheDir, StateDir string
	Method                         string
	Software                       []string
	Lock                           lockfile.Options
	Handlers                       map[string]reconcileHandler
}

// Report distinguishes source, preparation and each destination's work.
type Report struct {
	LockChanged bool             `json:"lock_changed"`
	Software    []SoftwareReport `json:"software"`
}

// SoftwareReport carries immutable facts and individual destination failures.
type SoftwareReport struct {
	Name           string              `json:"name"`
	SourceCached   bool                `json:"source_cached"`
	Prepared       *Prepared           `json:"prepared,omitempty"`
	Artifacts      map[string]Prepared `json:"artifacts,omitempty"`
	Steps          []StepReport        `json:"steps,omitempty"`
	ArtifactErrors map[string]string   `json:"artifact_errors,omitempty"`
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

// Run prepares locked inputs once and continues independent destinations after failures.
func Run(ctx context.Context, opts Options) (Report, error) {
	var report Report
	switch opts.Method {
	case "update", "prepare", "plan", "apply":
	default:
		return report, fmt.Errorf("unsupported run method %q", opts.Method)
	}
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return report, err
	}
	if err := Validate(ctx, p); err != nil {
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
	m := source.New(store, root, opts.Lock.Offline)
	pluginWork, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "operations-*")
	if err != nil {
		return report, err
	}
	defer func() { _ = os.RemoveAll(pluginWork) }()
	var ops *operations
	if opts.Method != "update" {
		ops, err = loadOperations(ctx, p, m, pluginWork, opts.Handlers)
		if err != nil {
			return report, err
		}
		if err := checkOperations(p, ops); err != nil {
			return report, err
		}
	}
	locked, err := lockfile.Prepare(ctx, p, m, opts.Lock)
	if err != nil {
		return report, err
	}
	report.LockChanged = locked.Changed
	if opts.Method == "update" {
		return report, nil
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(root, ".stemma", "state")
	}
	statePath := filepath.Join(stateDir, p.Project+".json")
	if opts.Method == "apply" {
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
	selected := opts.Software
	if len(selected) == 0 {
		for name := range p.Software {
			selected = append(selected, name)
		}
		sort.Strings(selected)
	}
	for _, name := range selected {
		if _, exists := p.Software[name]; !exists {
			return report, fmt.Errorf("unknown software %q", name)
		}
	}
	var failures []error
	for _, name := range selected {
		item := SoftwareReport{Name: name, SourceCached: locked.CacheHits[name]}
		work, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "run-*")
		if err != nil {
			return report, err
		}
		software := p.Software[name]
		preparation := software
		// Payload requirements apply to the delivered representations. The source
		// requirement is checked against the original acquired input only once.
		if software.Verification.Subject != "source" {
			preparation.Verification = config.Verification{}
		}
		if software.Source != nil {
			var prepared Prepared
			prepared, err = prepare(ctx, store, locked.File.Software[name], preparation, work)
			item.Prepared = &prepared
		}
		if err == nil {
			var outputs map[string]Prepared
			outputs, err = prepareSoftware(ctx, store, ops, software, item.Prepared, work, opts.Method == "prepare", &item)
			if err == nil {
				err = reconcileSoftware(ctx, opts, p, ops, store, root, work, name, outputs, &current, statePath, &item)
			}
		}
		if err != nil {
			item.Error = err.Error()
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}
		if err := os.RemoveAll(work); err != nil {
			failures = append(failures, err)
		}
		report.Software = append(report.Software, item)
	}
	return report, errors.Join(failures...)
}

type destinationInput struct {
	name     string
	prepared Prepared
	request  plugin.ReconcileRequest
	report   DestinationReport
}

func reconcileSoftware(ctx context.Context, opts Options, p config.Project, ops *operations, store *cas.Store, root, work, name string, outputs map[string]Prepared, current *state, statePath string, item *SoftwareReport) error {
	software := p.Software[name]
	inputs := []destinationInput{}
	var failures []error
	for _, destination := range sortedKeys(software.Destinations) {
		prepared, present := outputs["prepared"]
		if reference, ok := software.Destinations[destination]["artifact"].(string); ok {
			prepared, present = outputs[reference]
			if !present {
				failures = append(failures, fmt.Errorf("destination %s references missing output %s", destination, reference))
				continue
			}
		}
		var err error
		d := p.Destinations[destination]
		metadata := destinationMetadata(software.Destinations[destination])
		if present && !prepared.SuppliedFacts && needsInspection(metadata, d.Operation) {
			prepared.Facts, err = inspection.Read(ctx, prepared.Path)
			if err != nil {
				failures = append(failures, fmt.Errorf("destination %s required inspection: %w", destination, err))
				continue
			}
		}
		effective, origins, err := resolveMetadata(software, metadata, prepared.Facts, d.Operation)
		if err != nil {
			failures = append(failures, fmt.Errorf("destination %s metadata: %w", destination, err))
			continue
		}
		if present {
			prepared, err = materialize(ctx, store, prepared, filepath.Join(work, "destinations", destination))
			if err != nil {
				return err
			}
		}
		input, err := makeDestinationInput(p, root, name, destination, prepared, effective, origins, current)
		if err == nil {
			input.request.Inputs = map[string]plugin.Artifact{}
			for inputName, reference := range destinationReferences(software.Destinations[destination]) {
				artifact, exists := outputs[reference]
				if !exists {
					err = fmt.Errorf("missing input %s output %s", inputName, reference)
					break
				}
				artifact, err = materialize(ctx, store, artifact, filepath.Join(work, "destinations", destination, "inputs", inputName))
				if err != nil {
					break
				}
				input.request.Inputs[inputName] = artifact.artifact()
			}
		}
		if err == nil {
			err = ops.call(ctx, d.Operation, "validate", input.request, nil)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("destination %s: %w", destination, err))
			continue
		}
		inputs = append(inputs, input)
	}
	// Every configured output must be valid before any publication. Apply still
	// re-observes each destination and retains partial bindings independently.
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	for _, input := range inputs {
		if err := verifyLeases(ctx, store, work, input.request); err != nil {
			return err
		}
	}
	if opts.Method == "prepare" {
		return nil
	}
	for i := range inputs {
		input := &inputs[i]
		input.request.Method = "plan"
		var response plugin.ReconcileResponse
		err := ops.call(ctx, p.Destinations[input.name].Operation, "plan", input.request, &response)
		input.report.Changes = response.Changes
		if err != nil {
			input.report.Error = err.Error()
			failures = append(failures, fmt.Errorf("destination %s: %w", input.name, err))
		}
	}
	for _, input := range inputs {
		if err := verifyLeases(ctx, store, work, input.request); err != nil {
			return err
		}
	}
	if opts.Method == "plan" {
		for _, input := range inputs {
			item.Destinations = append(item.Destinations, input.report)
		}
		return errors.Join(failures...)
	}
	for _, input := range inputs {
		if input.report.Error != "" {
			item.Destinations = append(item.Destinations, input.report)
			continue
		}
		report, err := deliver(ctx, ops, p, store, work, name, input, current, statePath)
		if err != nil {
			report.Error = err.Error()
			failures = append(failures, fmt.Errorf("destination %s: %w", input.name, err))
		}
		item.Destinations = append(item.Destinations, report)
	}
	return errors.Join(failures...)
}

func makeDestinationInput(p config.Project, root, software, name string, prepared Prepared, metadata map[string]any, origins map[string]string, current *state) (destinationInput, error) {
	d := p.Destinations[name]
	previous := current.Bindings[software+"/"+name]
	if previous.Connection != d.Fingerprint() {
		previous = binding{Connection: d.Fingerprint()}
	}
	input := destinationInput{name: name, prepared: prepared, report: DestinationReport{Name: name, Origins: origins, SourceChanged: previous.Source != prepared.Source.Artifact.SHA256, PreparedChanged: previous.Payload != prepared.Payload.SHA256}}
	if prepared.Tree && nativeHandler(d.Operation) != nil {
		return input, fmt.Errorf("destination %s requires a file representation; %s conversion is unsupported", name, prepared.Format)
	}
	metadataData, err := json.Marshal(metadata)
	if err != nil {
		return input, err
	}
	settings := d.Config
	if d.Operation == "munki" {
		path := d.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		settings = map[string]any{"path": path}
	}
	configData, err := json.Marshal(settings)
	if err != nil {
		return input, err
	}
	input.request = plugin.ReconcileRequest{Method: "validate", Identity: plugin.Identity{Project: p.Project, Recipe: software, Destination: name}, Config: configData, Metadata: metadataData, Artifact: prepared.artifact(), Facts: prepared.Facts, Binding: previous.Binding}
	return input, nil
}

func deliver(ctx context.Context, ops *operations, p config.Project, store *cas.Store, work, software string, input destinationInput, current *state, statePath string) (DestinationReport, error) {
	d := p.Destinations[input.name]
	report := input.report
	previous := current.Bindings[software+"/"+input.name]
	if previous.Connection != d.Fingerprint() {
		previous = binding{Connection: d.Fingerprint()}
	}
	if err := verifyLeases(ctx, store, work, input.request); err != nil {
		return report, err
	}
	input.request.Method = "apply"
	var response plugin.ReconcileResponse
	err := ops.call(ctx, d.Operation, "apply", input.request, &response)
	err = errors.Join(err, verifyLeases(ctx, store, work, input.request))
	report.Changes = response.Changes
	if err == nil || len(response.Binding) > 0 {
		if len(response.Binding) > 0 {
			previous.Binding = response.Binding
		}
		if err == nil {
			previous.Source = input.prepared.Source.Artifact.SHA256
			previous.Payload = input.prepared.Payload.SHA256
			report.Applied = true
		}
		current.Bindings[software+"/"+input.name] = previous
		if saveErr := saveState(statePath, *current); saveErr != nil {
			return report, errors.Join(err, saveErr)
		}
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
		if ref.SHA256 != artifact.SHA256 || ref.Size != artifact.Size {
			return errors.New("operation modified an immutable leased artifact")
		}
	}
	return nil
}

// Validate checks authored native fields and references without acquisition
// or destination requests. Prepared input validation runs later.
func Validate(ctx context.Context, p config.Project) error {
	for name, software := range p.Software {
		if err := validateReferences(software); err != nil {
			return fmt.Errorf("software %s: %w", name, err)
		}
		for destination, metadata := range software.Destinations {
			d := p.Destinations[destination]
			handler := nativeHandler(d.Operation)
			if handler == nil {
				continue
			}
			settings := d.Config
			if d.Operation == "munki" {
				settings = map[string]any{"path": d.Path}
			}
			configData, err := json.Marshal(settings)
			if err != nil {
				return err
			}
			static := destinationMetadata(metadata)
			for key, value := range static {
				if hasFactReference(value) {
					delete(static, key)
				}
			}
			metadataData, err := json.Marshal(static)
			if err != nil {
				return err
			}
			if d.Operation == "munki" {
				err = munkirepo.Validate(configData, metadataData)
			} else {
				_, err = handler(ctx, plugin.ReconcileRequest{Method: "validate", Config: configData, Metadata: metadataData})
			}
			if err != nil {
				return fmt.Errorf("%s/%s: %w", name, destination, err)
			}
		}
	}
	return nil
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
	s := state{Version: 1, Project: project, Bindings: map[string]binding{}}
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
	if s.Version != 1 || s.Project != project || s.Bindings == nil {
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
