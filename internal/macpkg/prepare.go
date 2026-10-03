package macpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/artifactname"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

// Prepare evaluates the package layout with input metadata, then builds.
func Prepare(ctx context.Context, request plugin.ResourceRequest[json.RawMessage]) (plugin.Artifact, error) {
	var raw map[string]any
	if err := json.Unmarshal(request.Config, &raw); err != nil {
		return plugin.Artifact{}, err
	}
	sources := newSources(request.Inputs, request.Workspace)
	defer sources.close()
	inspected, all, err := expression.Uses(raw, "inputs", "facts")
	if err != nil {
		return plugin.Artifact{}, err
	}
	// Only portable artifact data is visible; leased runner paths are not expressions.
	inputs := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(request.Inputs)) {
		input := request.Inputs[name]
		evidence := input.Evidence
		if evidence == nil {
			evidence = map[string]json.RawMessage{}
		}
		fields := map[string]any{"version": input.Version, "filename": input.Filename, "sha256": input.SHA256, "size": input.Size, "format": input.Format, "evidence": evidence}
		if all || slices.Contains(inspected, name) {
			if fields["facts"], err = inputFacts(ctx, sources, name); err != nil {
				return plugin.Artifact{}, fmt.Errorf("input %q: %w", name, err)
			}
		}
		inputs[name] = fields
	}
	environment := request.Environment
	if environment == nil {
		environment = map[string]string{}
	}
	data, err := json.Marshal(map[string]any{"env": environment, "inputs": inputs})
	if err != nil {
		return plugin.Artifact{}, err
	}
	var contexts map[string]any
	if err := json.Unmarshal(data, &contexts); err != nil {
		return plugin.Artifact{}, err
	}
	resolved, err := expression.Eval(raw, contexts)
	if err != nil {
		if errors.Is(err, expression.ErrMissingReference) {
			for _, name := range slices.Sorted(maps.Keys(inputs)) {
				fields := inputs[name].(map[string]any)
				if facts, ok := fields["facts"].(map[string]plugin.Subject); ok {
					err = fmt.Errorf("%w; input %q available subject IDs: %s", err, name, inspect.FormatSubjectIDs(slices.Sorted(maps.Keys(facts))))
				}
			}
		}
		return plugin.Artifact{}, err
	}
	object := resolved.(map[string]any)
	if err := validatePreparedReferences(raw, object); err != nil {
		return plugin.Artifact{}, err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return plugin.Artifact{}, err
	}
	schema, err := json.Marshal(plugin.SchemaFor[Spec]())
	if err != nil {
		return plugin.Artifact{}, err
	}
	if err := plugin.ValidateSchema(schema, encoded); err != nil {
		return plugin.Artifact{}, fmt.Errorf("resolved package config: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal(encoded, &spec); err != nil {
		return plugin.Artifact{}, err
	}
	if spec.Package.Filename == "" {
		spec.Package.Filename = artifactname.Filename(request.Identity.Name, spec.Package.Version, "", "pkg")
	}
	// Expressions have resolved; verify precisely what this layout consumes.
	spec.Inputs = make(map[string]plugin.Input, len(request.Inputs))
	for name := range request.Inputs {
		spec.Inputs[name] = plugin.Input{}
	}
	if err := spec.Validate(); err != nil {
		return plugin.Artifact{}, err
	}
	var observed []signature.Observation
	if len(spec.Signatures) > 0 || request.Derive == "signature" {
		observed, err = verifyInputs(ctx, sources, spec, request.Derive == "signature")
		if err != nil {
			return plugin.Artifact{}, err
		}
	}
	artifact, err := build(ctx, spec, sources, request.Workspace)
	if err != nil || len(observed) == 0 {
		return artifact, err
	}
	encoded, err = json.Marshal(observed)
	if err != nil {
		return plugin.Artifact{}, err
	}
	artifact.Evidence = map[string]json.RawMessage{"signatures": encoded}
	return artifact, nil
}

func verifyInputs(ctx context.Context, sources *sources, spec Spec, derive bool) ([]signature.Observation, error) {
	selections := map[string][]string{}
	for _, entry := range spec.Payload {
		if entry.Input != "" {
			selections[entry.Input] = append(selections[entry.Input], entry.Path)
		}
	}
	for _, entry := range spec.Scripts {
		if entry.Input != "" {
			selections[entry.Input] = append(selections[entry.Input], entry.Path)
		}
	}
	policies := map[string][]signature.Expectation{}
	if !derive {
		for _, policy := range spec.Signatures {
			if _, consumed := selections[policy.Input]; !consumed {
				return nil, fmt.Errorf("signature input %q is not consumed by the build", policy.Input)
			}
			policies[policy.Input] = append(policies[policy.Input], policy.Expectation)
		}
	}
	var observations []signature.Observation
	for _, name := range slices.Sorted(maps.Keys(selections)) {
		source, err := sources.get(ctx, name)
		if err != nil {
			return nil, err
		}
		targets := map[string]plugin.Subject{}
		paths := selections[name]
		slices.Sort(paths)
		for _, selection := range slices.Compact(paths) {
			facts, err := inspect.Selection(ctx, source, selection)
			if err != nil {
				return nil, fmt.Errorf("input %q path %q: %w", name, selection, err)
			}
			// A scalar PKG has one outer signing subject, never its component
			// receipts or installed payload apps. A selected app is likewise whole.
			root := facts.Subjects[0]
			filename := selection
			if filename == "" || filename == "." {
				filename = source.Artifact().Filename
			}
			packageRoot := root.Kind == "container" && (strings.EqualFold(path.Ext(filename), ".pkg") || slices.ContainsFunc(facts.Subjects, func(subject plugin.Subject) bool { return subject.Parent == root.ID && subject.Package != nil }))
			if root.App != nil || packageRoot {
				targets[root.Path] = root
				continue
			}
			for _, subject := range facts.Subjects[1:] {
				if subject.Parent == root.ID && (subject.App != nil || subject.Kind == "container" && strings.EqualFold(path.Ext(subject.Path), ".pkg")) {
					targets[subject.Path] = subject
				}
			}
		}
		subjects := make([]plugin.Subject, 0, len(targets))
		for _, name := range slices.Sorted(maps.Keys(targets)) {
			subjects = append(subjects, targets[name])
		}
		observed, err := signature.Verify(ctx, policies[name], subjects, derive, func(subject plugin.Subject) (signature.Result, error) {
			return apple.VerifySubject(ctx, source, subject, sources.workspace)
		})
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}
		for i := range observed {
			observed[i].Input = name
		}
		observations = append(observations, observed...)
	}
	return observations, nil
}

// inputFacts inventories an input and keys its subjects by ID, as destination
// facts are.
func inputFacts(ctx context.Context, sources *sources, name string) (map[string]plugin.Subject, error) {
	facts, err := sources.inventory(ctx, name)
	if err != nil {
		return nil, err
	}
	subjects := make(map[string]plugin.Subject, len(facts.Subjects))
	for _, subject := range facts.Subjects {
		subjects[subject.ID] = subject
	}
	return subjects, nil
}

func validatePreparedReferences(raw, resolved map[string]any) error {
	for _, area := range []string{"payload", "scripts"} {
		declared, _ := raw[area].(map[string]any)
		entries, _ := resolved[area].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			entry, _ := entries[name].(map[string]any)
			original, _ := declared[name].(map[string]any)
			if entry["$input"] != original["$input"] {
				return fmt.Errorf("%s %q: $input references must be declared before preparation", area, name)
			}
		}
	}
	return nil
}
