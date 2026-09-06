package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// StepReport retains every named result without promoting one artifact's facts
// into another artifact's identity.
type StepReport struct {
	Name      string              `json:"name"`
	Operation string              `json:"operation"`
	Artifacts map[string]Prepared `json:"artifacts,omitempty"`
	Facts     plugin.Facts        `json:"facts,omitzero"`
	Cached    bool                `json:"cached"`
}

func (o *operations) check(name string, preparation bool) error {
	op, err := o.operation(name)
	if err != nil {
		return err
	}
	methods := []string{"validate", "plan", "apply"}
	if preparation {
		methods = []string{"validate", "run"}
		if op.SideEffects == "remote" {
			return fmt.Errorf("operation %s declares remote effects and cannot run during preparation", name)
		}
	}
	for _, method := range methods {
		if !op.SupportsMethod(method) {
			return fmt.Errorf("operation %s must support %s", name, method)
		}
	}
	if !op.SupportsPlatform(runtime.GOOS, runtime.GOARCH) {
		return fmt.Errorf("operation %s does not support %s/%s", name, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func (p Prepared) artifact() plugin.Artifact {
	return plugin.Artifact{Path: p.Path, SHA256: p.Payload.SHA256, Size: p.Payload.Size, Filename: p.Filename, Format: p.Format, Version: p.Version, Tree: p.Tree, Facts: p.Facts}
}

func runStep(ctx context.Context, store *cas.Store, ops *operations, step config.Step, inputs map[string]Prepared, entry source.Entry, work string) (StepReport, error) {
	result := StepReport{Name: step.Name, Operation: step.Operation}
	if err := ops.check(step.Operation, true); err != nil {
		return result, err
	}
	identityInputs := map[string]plugin.Artifact{}
	for name, input := range inputs {
		artifact := input.artifact()
		artifact.Path = ""
		identityInputs[name] = artifact
	}
	key := config.Fingerprint(struct {
		Implementation, Operation, Provider string
		Config                              map[string]any
		Inputs                              map[string]plugin.Artifact
		Timestamp                           time.Time
	}{"step/1", step.Operation, ops.identity[step.Operation], step.Config, identityInputs, entry.ResolvedAt})
	if descriptor, ok := store.Recall(ctx, key); ok {
		path, err := store.Path(descriptor)
		if err != nil {
			return result, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return result, err
		}
		var cached StepReport
		valid := json.Unmarshal(data, &cached) == nil
		for name, artifact := range cached.Artifacts {
			valid = valid && safeOutputName(name) && safeFilename(artifact.Filename) && store.Verify(ctx, artifact.Payload) == nil
		}
		if valid {
			cached.Name, cached.Operation, cached.Cached = step.Name, step.Operation, true
			for name, artifact := range cached.Artifacts {
				artifact.Source, artifact.Cached = entry, true
				artifact, err = materialize(ctx, store, artifact, filepath.Join(work, "cached", name))
				if err != nil {
					return result, err
				}
				cached.Artifacts[name] = artifact
			}
			return cached, nil
		}
	}
	workspace := filepath.Join(work, "output")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return result, err
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return result, err
	}
	configData, err := json.Marshal(step.Config)
	if err != nil {
		return result, err
	}
	request := plugin.StepRequest{Config: configData, Inputs: map[string]plugin.Artifact{}, Workspace: workspace, Timestamp: entry.ResolvedAt}
	for name, input := range inputs {
		leased, err := materialize(ctx, store, input, filepath.Join(work, "inputs", name))
		if err != nil {
			return result, err
		}
		leased.Path, err = filepath.EvalSymlinks(leased.Path)
		if err != nil {
			return result, err
		}
		request.Inputs[name] = leased.artifact()
	}
	if err := ops.call(ctx, step.Operation, "validate", request, nil); err != nil {
		return result, err
	}
	var output plugin.StepResponse
	if err := ops.call(ctx, step.Operation, "run", request, &output); err != nil {
		return result, err
	}
	// Plugins are trusted executables, but accidental input mutation is still a
	// contract failure. Private copies prevent it from corrupting the cache.
	for name, input := range request.Inputs {
		ref, err := importPath(ctx, store, input.Path, input.Tree, work)
		if err != nil {
			return result, err
		}
		if ref != inputs[name].Payload {
			return result, fmt.Errorf("operation %s modified immutable input %s", step.Operation, name)
		}
	}
	if err := validateFacts(output.Facts); err != nil {
		return result, err
	}
	result.Facts = output.Facts
	result.Artifacts = map[string]Prepared{}
	for name, artifact := range output.Artifacts {
		if !safeOutputName(name) {
			return result, fmt.Errorf("operation %s returned unsafe output name %q", step.Operation, name)
		}
		if artifact.Filename == "" {
			artifact.Filename = filepath.Base(artifact.Path)
		}
		if !safeFilename(artifact.Filename) {
			return result, fmt.Errorf("operation %s returned unsafe filename", step.Operation)
		}
		resolved, err := filepath.EvalSymlinks(artifact.Path)
		if err != nil {
			return result, err
		}
		allowed := within(workspace, resolved)
		for _, input := range request.Inputs {
			if resolved == input.Path {
				allowed = true
			}
		}
		if !allowed {
			return result, fmt.Errorf("operation %s output %s is outside its workspace and leased inputs", step.Operation, name)
		}
		observed, err := inspect(ctx, resolved)
		if err != nil {
			return result, err
		}
		if artifact.Tree != observed.Tree {
			return result, fmt.Errorf("operation %s output %s has incorrect tree flag", step.Operation, name)
		}
		if artifact.Format != "" {
			if !safeOutputName(artifact.Format) {
				return result, fmt.Errorf("operation %s output %s has invalid format", step.Operation, name)
			}
			observed.Format = artifact.Format
		}
		observed.Filename = artifact.Filename
		observed.Source = entry
		observed.Payload, err = importPath(ctx, store, resolved, observed.Tree, work)
		if err != nil {
			return result, err
		}
		if artifact.SHA256 != "" && (artifact.SHA256 != observed.Payload.SHA256 || artifact.Size != observed.Payload.Size) {
			return result, fmt.Errorf("operation %s output %s digest or size differs from its bytes", step.Operation, name)
		}
		if artifact.Facts.Version != 0 {
			if err := validateFacts(artifact.Facts); err != nil {
				return result, err
			}
			observed.Facts = artifact.Facts
			observed.SuppliedFacts = true
		}
		// Consumer-selected versions belong to native metadata; observed facts
		// continue to determine an artifact's version convenience value.
		observed.Version = artifactVersion(observed.Facts)
		result.Artifacts[name] = observed
	}
	data, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	descriptor, err := store.Import(ctx, bytes.NewReader(data), "")
	if err != nil {
		return result, err
	}
	return result, store.Remember(key, descriptor)
}

func importPath(ctx context.Context, store *cas.Store, path string, tree bool, work string) (cas.Ref, error) {
	if !tree {
		return store.ImportFile(ctx, path, "")
	}
	packed, err := os.CreateTemp(work, "tree-*.tar")
	if err != nil {
		return cas.Ref{}, err
	}
	defer func() { _ = os.Remove(packed.Name()) }()
	err = archive.Pack(ctx, path, packed)
	err = errors.Join(err, packed.Close())
	if err != nil {
		return cas.Ref{}, err
	}
	return store.ImportFile(ctx, packed.Name(), "")
}

var outputName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func safeOutputName(name string) bool {
	return outputName.MatchString(name) && name != "." && name != ".."
}
func safeFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:") && strings.IndexFunc(name, unicode.IsControl) < 0
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

func validateFacts(facts plugin.Facts) error {
	if facts.Version == 0 && len(facts.Subjects) == 0 {
		return nil
	}
	if facts.Version != plugin.FactsVersion {
		return errors.New("unsupported artifact facts version")
	}
	ids := map[string]bool{}
	for _, subject := range facts.Subjects {
		if subject.ID == "" || ids[subject.ID] || subject.Kind == "" {
			return errors.New("artifact facts require unique subject IDs and kinds")
		}
		if subject.Path != "" && (!filepath.IsLocal(filepath.FromSlash(subject.Path)) || strings.Contains(subject.Path, "\\")) {
			return errors.New("artifact subject path must be relative to its artifact")
		}
		if subject.InstalledPath != "" && !strings.HasPrefix(subject.InstalledPath, "/") {
			return errors.New("installed subject path must be absolute")
		}
		ids[subject.ID] = true
	}
	for _, subject := range facts.Subjects {
		if subject.Parent != "" && (!ids[subject.Parent] || subject.Parent == subject.ID) {
			return errors.New("artifact subject has an invalid parent")
		}
	}
	return nil
}
