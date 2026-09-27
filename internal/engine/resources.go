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
	"strings"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/plugin"
)

type resourcePlan struct {
	plugin.ResourceResult

	Resource    config.Resource
	Operation   string
	Environment map[string]string
}

// selectResources resolves selectors against declared identity alone, so the
// selector is an execution boundary rather than a filter applied after the
// whole catalog has been evaluated. An empty selection takes every unsuspended
// resource; a suspended resource becomes a root only through a selector naming
// it or a selected resource consuming its outputs.
func selectResources(resources map[string]config.Resource, selectors []string) ([]string, error) {
	if len(selectors) == 0 {
		var roots []string
		for _, key := range sortedKeys(resources) {
			if !resources[key].Suspend {
				roots = append(roots, key)
			}
		}
		return roots, nil
	}
	var roots []string
	for _, selection := range selectors {
		if _, declared := resources[selection]; declared {
			roots = append(roots, selection)
			continue
		}
		var matches []string
		for _, key := range sortedKeys(resources) {
			resource := resources[key]
			if resource.Metadata.Name == selection || resource.Kind+"/"+resource.Metadata.Name == selection {
				matches = append(matches, key)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("selection %q matches %d resources; use Kind/name or apiVersion/Kind/name", selection, len(matches))
		}
		roots = append(roots, matches[0])
	}
	return roots, nil
}

// discoverClosure evaluates the requested resources and everything they
// consume, and returns the evaluated plans alongside the roots' closure in
// dependency-first order. Nothing outside the closure is evaluated, so a
// resource that fails its own operation contract cannot fail a run that does
// not reach it. Environment reads the closure's environment values for a run
// that acquires it; publication peers only lend metadata, so their
// declarations always stay as written.
func discoverClosure(ctx context.Context, p config.Project, ops *operations, roots []string, environment bool) (map[string]resourcePlan, []string, error) {
	kinds := map[plugin.ResourceKind]plugin.Operation{}
	for _, op := range ops.registry.Descriptor().Operations {
		if op.Resource != nil {
			kinds[*op.Resource] = op
		}
	}
	plans := map[string]resourcePlan{}
	// Callers reach evaluate with declared keys: selection resolves roots
	// against the project, resource inputs name their producer before the walk
	// follows it, and publication references are checked before they join it.
	evaluate := func(key string, environment bool) error {
		if _, evaluated := plans[key]; evaluated {
			return nil
		}
		plan, err := discoverResource(ctx, p, ops, kinds, key, p.Resources[key], environment)
		if err != nil {
			return err
		}
		plans[key] = plan
		return nil
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
		if err := evaluate(key, environment); err != nil {
			return err
		}
		active[key] = true
		for _, name := range sortedKeys(plans[key].Inputs) {
			if ref := plans[key].Inputs[name].Resource; ref != nil {
				if err := visit(ref.Key()); err != nil {
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
			return nil, nil, err
		}
	}
	// A publication reference contributes peer metadata to a destination in the
	// closure, so its resource is evaluated without joining the run.
	// planDestinations owns whether the references themselves are valid.
	for queue := slices.Clone(ordered); len(queue) > 0; {
		key := queue[0]
		queue = queue[1:]
		for _, destination := range sortedKeys(plans[key].Destinations) {
			references, _ := publicationReferences(destinationMetadata(plans[key].Destinations[destination]))
			for _, reference := range references {
				peer := reference.Key()
				if _, evaluated := plans[peer]; evaluated {
					continue
				}
				if _, declared := p.Resources[peer]; !declared {
					continue
				}
				if err := evaluate(peer, false); err != nil {
					return nil, nil, err
				}
				queue = append(queue, peer)
			}
		}
	}
	return plans, ordered, nil
}

// discoverResource evaluates one resource against its registered kind and
// validates everything the declaration owns: its configuration, the resolvers
// of its non-resource inputs, the resources it consumes and the destinations it
// publishes to. Without the environment, the kind discovers the declaration
// as written and values that hold expressions are checked by schema alone.
func discoverResource(ctx context.Context, p config.Project, ops *operations, kinds map[plugin.ResourceKind]plugin.Operation, key string, r config.Resource, environment bool) (resourcePlan, error) {
	op, ok := kinds[plugin.ResourceKind{APIVersion: r.APIVersion, Kind: r.Kind}]
	if !ok {
		return resourcePlan{}, fmt.Errorf("resource %s: no installed operation registers this apiVersion and kind", key)
	}
	if err := ops.check(op.Name, true); err != nil {
		return resourcePlan{}, err
	}
	declaration, bindings, err := resourceDeclaration(r, environment)
	if err != nil {
		return resourcePlan{}, fmt.Errorf("resource %s: %w", key, err)
	}
	encoded, err := json.Marshal(declaration)
	if err != nil {
		return resourcePlan{}, err
	}
	var result plugin.ResourceResult
	schema := op.ConfigSchema
	if !environment || deferredPreparation(r) {
		schema, err = config.ExpressionSchema(schema)
		if err != nil {
			return resourcePlan{}, err
		}
	}
	if len(schema) > 0 {
		if err := plugin.ValidateSchema(schema, encoded); err != nil {
			return resourcePlan{}, fmt.Errorf("resource %s config: %w", key, err)
		}
	}
	if err := ops.call(ctx, op.Name, "discover", plugin.ResourceRequest[json.RawMessage]{Config: encoded, Identity: r.Reference()}, &result); err != nil {
		return resourcePlan{}, fmt.Errorf("resource %s: %w", key, err)
	}
	if len(result.Config) == 0 {
		return resourcePlan{}, fmt.Errorf("resource %s: kind did not return preparation configuration", key)
	}
	for inputName, input := range result.Inputs {
		if inputName == "" {
			return resourcePlan{}, fmt.Errorf("resource %s has an unnamed input", key)
		}
		input.Base = r.Base
		result.Inputs[inputName] = input
		if input.Resource == nil {
			if err := ops.validateInput(ctx, input); err != nil {
				return resourcePlan{}, fmt.Errorf("resource %s input %s: %w", key, inputName, err)
			}
		}
		if input.Resource != nil {
			ref := input.Resource
			producer, ok := p.Resources[ref.Key()]
			if !ok {
				return resourcePlan{}, fmt.Errorf("resource %s input %s: unknown resource %s", key, inputName, ref.Key())
			}
			if producer.Suspend && !r.Suspend {
				return resourcePlan{}, fmt.Errorf("resource %s input %s: depends on suspended resource %s; suspend %s as well", key, inputName, ref.Key(), key)
			}
			if ref.Output != "" && !safeOutputName(ref.Output) {
				return resourcePlan{}, errors.New("invalid resource output name")
			}
		}
	}
	for destination := range result.Destinations {
		d, ok := p.Destinations[destination]
		if !ok {
			return resourcePlan{}, fmt.Errorf("resource %s: unknown destination %s", key, destination)
		}
		if err := ops.check(d.Operation, false); err != nil {
			return resourcePlan{}, err
		}
		if err := ops.configuration(d.Operation, d.Config, false); err != nil {
			return resourcePlan{}, fmt.Errorf("destination %s: %w", destination, err)
		}
	}
	for destination, metadata := range result.Destinations {
		if err := expression.Check(destinationMetadata(metadata), "env", "facts", "evidence"); err != nil {
			return resourcePlan{}, fmt.Errorf("resource %s destination %s: %w", key, destination, err)
		}
	}
	return resourcePlan{Resource: r, Operation: op.Name, ResourceResult: result, Environment: bindings}, nil
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

// prepareResource reuses cached outputs unless derive names a policy to
// observe, which always runs the operation and keeps its outputs out of the
// cache.
func prepareResource(ctx context.Context, store *cas.Store, ops *operations, plan resourcePlan, inputs map[string]Prepared, work string, derive string) (result map[string]Prepared, cacheHit bool, err error) {
	done := plugin.Stage(ctx, "Checking preparation cache")
	// The deferred call finishes whichever stage is current, with the outputs it found.
	var detail string
	defer func() { done(err, plugin.Detail(detail)) }()
	identityInputs := map[string]plugin.Artifact{}
	modes := map[string]uint32{}
	for name, input := range inputs {
		artifact := input.artifact()
		artifact.Path = ""
		identityInputs[name] = artifact
		modes[name] = input.Mode
	}
	key := config.Fingerprint(struct {
		Implementation, Provider string
		Identity                 plugin.ResourceReference
		Config                   json.RawMessage
		Inputs                   map[string]plugin.Artifact
		Modes                    map[string]uint32
		Environment              map[string]string
	}{"resource/2", ops.identity[plan.Operation], plan.Resource.Reference(), plan.Config, identityInputs, modes, plan.Environment})
	cached, complete, err := recallOutputs(ctx, store, key)
	if err != nil {
		return nil, false, err
	}
	if complete && derive == "" {
		for name, artifact := range cached {
			artifact.Cached = true
			artifact, err = materialize(ctx, store, artifact, filepath.Join(work, "cached", name))
			if err != nil {
				return nil, false, err
			}
			artifact.Path, err = filepath.EvalSymlinks(artifact.Path)
			if err != nil {
				return nil, false, err
			}
			cached[name] = artifact
		}
		detail = strings.TrimSpace(outputDetail(cached) + " (cached)")
		return cached, true, nil
	}

	done(nil, plugin.Detail("not cached"))
	done = plugin.Stage(ctx, "Materializing inputs")
	workspace := filepath.Join(work, "output")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return nil, false, err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, false, err
	}
	request := plugin.ResourceRequest[json.RawMessage]{Config: plan.Config, Identity: plan.Resource.Reference(), Inputs: map[string]plugin.Artifact{}, Workspace: workspace, Derive: derive, Environment: plan.Environment}
	// Windows stores only the read-only bit, so compare modes against the lease.
	leasedModes := map[string]os.FileMode{}
	for name, input := range inputs {
		leased, err := materialize(ctx, store, input, filepath.Join(work, "inputs", config.Fingerprint(name)))
		if err != nil {
			return nil, false, err
		}
		leased.Path, err = filepath.EvalSymlinks(leased.Path)
		if err != nil {
			return nil, false, err
		}
		info, err := os.Stat(leased.Path)
		if err != nil {
			return nil, false, err
		}
		leasedModes[name] = info.Mode().Perm()
		request.Inputs[name] = leased.artifact()
	}
	var response plugin.ResourceResult
	done(nil)
	done = plugin.Stage(ctx, "Preparing outputs")
	runErr := ops.call(ctx, plan.Operation, "run", request, &response)
	done(runErr)
	done = plugin.Stage(ctx, "Verifying workspace")
	for name, input := range request.Inputs {
		ref, err := importPath(ctx, store, input.Path, input.Tree, work)
		if err != nil {
			return nil, false, errors.Join(runErr, err)
		}
		info, err := os.Stat(input.Path)
		if err != nil {
			return nil, false, errors.Join(runErr, err)
		}
		if ref != inputs[name].Payload || info.Mode().Perm() != leasedModes[name] {
			return nil, false, errors.Join(runErr, fmt.Errorf("resource %s modified immutable input %s", plan.Resource.Reference().Key(), name))
		}
	}
	done(nil)
	if runErr != nil {
		return nil, false, runErr
	}
	done = plugin.Stage(ctx, "Recording artifacts")
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
		observed.InputsHash = config.Fingerprint(struct {
			Artifacts map[string]plugin.Artifact
			Modes     map[string]uint32
		}{identityInputs, modes})
		outputs[name] = observed
	}
	detail = outputDetail(outputs)
	return outputs, false, rememberOutputs(ctx, store, key, outputs)
}

// outputDetail names a preparation's installer for progress displays.
func outputDetail(outputs map[string]Prepared) string {
	installer := outputs["installer"]
	return strings.TrimSpace(installer.Filename + " " + installer.Version)
}

func rememberOutputs(ctx context.Context, store *cas.Store, key string, outputs map[string]Prepared) error {
	data, err := json.Marshal(outputs)
	if err != nil {
		return err
	}
	descriptor, err := store.Import(ctx, bytes.NewReader(data), "")
	if err != nil {
		return err
	}
	return store.Remember(key, descriptor)
}

func recallOutputs(ctx context.Context, store *cas.Store, key string) (map[string]Prepared, bool, error) {
	empty := map[string]Prepared{}
	descriptor, ok := store.Recall(ctx, key)
	if !ok {
		return empty, false, nil
	}
	filename, err := store.Path(descriptor)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, false, err
	}
	var cached map[string]Prepared
	valid := json.Unmarshal(data, &cached) == nil && cached != nil
	for name, artifact := range cached {
		valid = valid && safeOutputName(name) && safeFilename(artifact.Filename) && store.Verify(ctx, artifact.Payload) == nil
	}
	if !valid {
		return empty, false, nil
	}
	return cached, true, nil
}
