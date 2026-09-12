package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

type resourcePlan struct {
	Resource  config.Resource
	Operation string
	plugin.ResourceResult
}

func discover(ctx context.Context, p config.Project, ops *operations) (map[string]resourcePlan, error) {
	plans := map[string]resourcePlan{}
	kinds := map[plugin.ResourceKind]plugin.Operation{}
	for _, op := range ops.registry.Descriptor().Operations {
		if op.Resource != nil {
			kinds[*op.Resource] = op
		}
	}
	names := map[string]string{}
	for _, key := range sortedKeys(p.Resources) {
		r := p.Resources[key]
		op, ok := kinds[plugin.ResourceKind{APIVersion: r.APIVersion, Kind: r.Kind}]
		if !ok {
			return nil, fmt.Errorf("resource %s: no installed operation registers this apiVersion and kind", key)
		}
		if err := ops.check(op.Name, true); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(r.Spec)
		if err != nil {
			return nil, err
		}
		var result plugin.ResourceResult
		if err := ops.call(ctx, op.Name, "validate", plugin.ResourceRequest{Config: encoded, Identity: r.Reference()}, &result); err != nil {
			return nil, fmt.Errorf("resource %s: %w", key, err)
		}
		if len(result.Config) == 0 {
			return nil, fmt.Errorf("resource %s: kind did not return preparation configuration", key)
		}
		for inputName, input := range result.Inputs {
			if inputName == "" {
				return nil, fmt.Errorf("resource %s has an unnamed input", key)
			}
			input.Base = r.Base
			result.Inputs[inputName] = input
			if input.Resource != nil {
				ref := input.Resource
				if _, ok := p.Resources[ref.Key()]; !ok {
					return nil, fmt.Errorf("resource %s input %s: unknown resource %s", key, inputName, ref.Key())
				}
				if ref.Output != "" && !safeOutputName(ref.Output) {
					return nil, errors.New("invalid resource output name")
				}
			}
		}
		for destination := range result.Destinations {
			d, ok := p.Destinations[destination]
			if !ok {
				return nil, fmt.Errorf("resource %s: unknown destination %s", key, destination)
			}
			if err := ops.check(d.Operation, false); err != nil {
				return nil, err
			}
			if err := ops.configuration(d.Operation, d.Config); err != nil {
				return nil, err
			}
			nativeKey := r.Metadata.Name + "/" + destination
			if previous := names[nativeKey]; previous != "" {
				return nil, fmt.Errorf("%s and %s publish the same name on %s", previous, key, destination)
			}
			names[nativeKey] = key
		}
		if err := validateReferences(result); err != nil {
			return nil, err
		}
		plans[key] = resourcePlan{r, op.Name, result}
	}
	return plans, nil
}

func orderResources(plans map[string]resourcePlan, selected []string) ([]string, error) {
	var roots []string
	if len(selected) == 0 {
		roots = sortedKeys(plans)
	} else {
		for _, selection := range selected {
			if _, ok := plans[selection]; ok {
				roots = append(roots, selection)
				continue
			}
			var matches []string
			for key, plan := range plans {
				if plan.Resource.Metadata.Name == selection || plan.Resource.Kind+"/"+plan.Resource.Metadata.Name == selection {
					matches = append(matches, key)
				}
			}
			if len(matches) != 1 {
				return nil, fmt.Errorf("selection %q matches %d resources; use Kind/name or apiVersion/Kind/name", selection, len(matches))
			}
			roots = append(roots, matches[0])
		}
	}
	var ordered []string
	active, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(key string) error {
		if active[key] {
			return fmt.Errorf("build input cycle at %s", key)
		}
		if done[key] {
			return nil
		}
		active[key] = true
		for _, name := range sortedKeys(plans[key].Inputs) {
			input := plans[key].Inputs[name]
			if input.Resource != nil {
				if err := visit(input.Resource.Key()); err != nil {
					return err
				}
			}
		}
		delete(active, key)
		done[key] = true
		ordered = append(ordered, key)
		return nil
	}
	for _, key := range roots {
		if err := visit(key); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func preflight(plans map[string]resourcePlan, selected []string, p config.Project, ops *operations) error {
	for _, key := range selected {
		plan := plans[key]
		op, _ := ops.operation(plan.Operation)
		requirements := append(slices.Clone(op.Requirements), plan.Requirements...)
		for _, input := range plan.Inputs {
			if input.Resource == nil {
				if resolver, err := ops.operation(input.Resolver); err == nil {
					if err := ops.check(resolver.Name, true); err != nil {
						return err
					}
					requirements = append(requirements, resolver.Requirements...)
				}
			}
		}
		for destination := range plan.Destinations {
			native, _ := ops.operation(p.Destinations[destination].Operation)
			requirements = append(requirements, native.Requirements...)
		}
		for _, requirement := range requirements {
			if len(requirement.Platforms) > 0 && !slices.Contains(requirement.Platforms, runtime.GOOS) && !slices.Contains(requirement.Platforms, runtime.GOOS+"/"+runtime.GOARCH) {
				return fmt.Errorf("%s requires %s for %s: %s", key, requirement.Platforms, requirement.Purpose, requirement.Setup)
			}
			if requirement.Command != "" {
				if _, err := exec.LookPath(requirement.Command); err != nil {
					return fmt.Errorf("%s requires %s for %s: %s", key, requirement.Command, requirement.Purpose, requirement.Setup)
				}
			}
		}
	}
	return nil
}

func prepareResource(ctx context.Context, store *cas.Store, ops *operations, plan resourcePlan, inputs map[string]Prepared, work string) (map[string]Prepared, bool, error) {
	plugin.Stage(ctx, "Checking preparation cache")
	identityInputs := map[string]plugin.Artifact{}
	modes := map[string]uint32{}
	var timestamp time.Time
	for name, input := range inputs {
		artifact := input.artifact()
		artifact.Path = ""
		identityInputs[name] = artifact
		modes[name] = input.Mode
		if input.Timestamp.After(timestamp) {
			timestamp = input.Timestamp
		}
	}
	if timestamp.IsZero() {
		timestamp = time.Unix(0, 0).UTC()
	}
	key := config.Fingerprint(struct {
		Implementation, Provider string
		Identity                 plugin.ResourceReference
		Config                   json.RawMessage
		Inputs                   map[string]plugin.Artifact
		Modes                    map[string]uint32
		Timestamp                time.Time
	}{"resource/1", ops.identity[plan.Operation], plan.Resource.Reference(), plan.Config, identityInputs, modes, timestamp})
	if descriptor, ok := store.Recall(ctx, key); ok {
		filename, err := store.Path(descriptor)
		if err != nil {
			return nil, false, err
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, false, err
		}
		var cached map[string]Prepared
		valid := json.Unmarshal(data, &cached) == nil
		for name, artifact := range cached {
			valid = valid && safeOutputName(name) && safeFilename(artifact.Filename) && store.Verify(ctx, artifact.Payload) == nil
		}
		if valid {
			plugin.Stage(ctx, "Restoring cached preparation")
			for name, artifact := range cached {
				artifact.Cached = true
				artifact, err = materialize(ctx, store, artifact, filepath.Join(work, "cached", name))
				if err != nil {
					return nil, false, err
				}
				cached[name] = artifact
			}
			return cached, true, nil
		}
	}
	workspace := filepath.Join(work, "output")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return nil, false, err
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, false, err
	}
	request := plugin.ResourceRequest{Config: plan.Config, Identity: plan.Resource.Reference(), Inputs: map[string]plugin.Artifact{}, Workspace: workspace, Timestamp: timestamp}
	plugin.Stage(ctx, "Materializing inputs")
	for name, input := range inputs {
		leased, err := materialize(ctx, store, input, filepath.Join(work, "inputs", config.Fingerprint(name)))
		if err != nil {
			return nil, false, err
		}
		leased.Path, err = filepath.EvalSymlinks(leased.Path)
		if err != nil {
			return nil, false, err
		}
		request.Inputs[name] = leased.artifact()
	}
	var response plugin.ResourceResult
	plugin.Stage(ctx, "Preparing outputs")
	runErr := ops.call(ctx, plan.Operation, "run", request, &response)
	plugin.Stage(ctx, "Verifying workspace")
	for name, input := range request.Inputs {
		ref, err := importPath(ctx, store, input.Path, input.Tree, work)
		if err != nil {
			return nil, false, errors.Join(runErr, err)
		}
		info, err := os.Stat(input.Path)
		if err != nil {
			return nil, false, errors.Join(runErr, err)
		}
		if ref != inputs[name].Payload || uint32(info.Mode().Perm()) != inputs[name].Mode {
			return nil, false, errors.Join(runErr, fmt.Errorf("resource %s modified immutable input %s", plan.Resource.Reference().Key(), name))
		}
	}
	if runErr != nil {
		return nil, false, runErr
	}
	outputs := map[string]Prepared{}
	for name, artifact := range response.Artifacts {
		if !safeOutputName(name) {
			return nil, false, fmt.Errorf("unsafe output name %q", name)
		}
		if artifact.Filename == "" {
			artifact.Filename = filepath.Base(artifact.Path)
		}
		if !safeFilename(artifact.Filename) {
			return nil, false, errors.New("unsafe artifact filename")
		}
		resolved, err := filepath.EvalSymlinks(artifact.Path)
		if err != nil {
			return nil, false, err
		}
		allowed := within(workspace, resolved)
		for _, input := range request.Inputs {
			if resolved == input.Path {
				allowed = true
			}
		}
		if !allowed {
			return nil, false, fmt.Errorf("output %s is outside the workspace and leased inputs", name)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, false, err
		}
		if info.IsDir() != artifact.Tree || !info.IsDir() && !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("output %s has incorrect file-or-tree representation", name)
		}
		observed := Prepared{Filename: artifact.Filename, Path: resolved, Tree: artifact.Tree, Mode: uint32(info.Mode().Perm()), Format: artifact.Format, Version: artifact.Version}
		if observed.Format == "" {
			observed.Format = "file"
			if observed.Tree {
				observed.Format = "directory"
			}
		}
		if !safeOutputName(observed.Format) {
			return nil, false, errors.New("invalid artifact format")
		}

		observed.Payload, err = importPath(ctx, store, resolved, observed.Tree, work)
		if err != nil {
			return nil, false, err
		}
		if artifact.SHA256 != "" && (artifact.SHA256 != observed.Payload.SHA256 || artifact.Size != observed.Payload.Size) {
			return nil, false, fmt.Errorf("output %s digest differs from its bytes", name)
		}
		if artifact.Facts.Version != 0 {
			if err := validateFacts(artifact.Facts); err != nil {
				return nil, false, err
			}
			observed.Facts = artifact.Facts
			observed.SuppliedFacts = true
		}
		observed.EntryPoint = artifact.EntryPoint
		observed.Evidence = artifact.Evidence
		observed.Timestamp = timestamp
		observed.InputsHash = config.Fingerprint(struct {
			Artifacts map[string]plugin.Artifact
			Modes     map[string]uint32
		}{identityInputs, modes})
		outputs[name] = observed
	}
	data, err := json.Marshal(outputs)
	if err != nil {
		return nil, false, err
	}
	descriptor, err := store.Import(ctx, bytes.NewReader(data), "")
	if err != nil {
		return nil, false, err
	}
	return outputs, false, store.Remember(key, descriptor)
}
