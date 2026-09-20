package macpkg

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/woodleighschool/stemma/internal/artifactname"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/plugin"
)

// Prepare evaluates the authored layout with input metadata, then builds.
func Prepare(ctx context.Context, request plugin.ResourceRequest[json.RawMessage]) (plugin.Artifact, error) {
	var raw map[string]any
	if err := json.Unmarshal(request.Config, &raw); err != nil {
		return plugin.Artifact{}, err
	}
	// Only portable artifact data is visible; leased runner paths are not expressions.
	inputs := map[string]any{}
	for name, input := range request.Inputs {
		evidence := input.Evidence
		if evidence == nil {
			evidence = map[string]json.RawMessage{}
		}
		inputs[name] = map[string]any{"version": input.Version, "filename": input.Filename, "sha256": input.SHA256, "size": input.Size, "format": input.Format, "evidence": evidence}
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
	return Build(ctx, spec, request.Inputs, request.Workspace, request.Timestamp)
}

func validatePreparedReferences(raw, resolved map[string]any) error {
	for _, area := range []string{"payload", "scripts"} {
		authored, _ := raw[area].(map[string]any)
		entries, _ := resolved[area].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			entry, _ := entries[name].(map[string]any)
			original, _ := authored[name].(map[string]any)
			if entry["$input"] != original["$input"] {
				return fmt.Errorf("%s %q: $input references must be declared before preparation", area, name)
			}
		}
	}
	return nil
}
