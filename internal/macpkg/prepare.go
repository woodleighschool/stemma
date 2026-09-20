package macpkg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/artifactname"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

// Prepare inspects nominated inputs, evaluates the authored layout, then builds.
// Inspections and copies share the same opened input contents.
func Prepare(ctx context.Context, request plugin.ResourceRequest[json.RawMessage]) (plugin.Artifact, error) {
	var raw map[string]any
	if err := json.Unmarshal(request.Config, &raw); err != nil {
		return plugin.Artifact{}, err
	}
	inspections, err := json.Marshal(raw["inspect"])
	if err != nil {
		return plugin.Artifact{}, err
	}
	var selections map[string]Inspection
	decoder := json.NewDecoder(bytes.NewReader(inspections))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selections); err != nil {
		return plugin.Artifact{}, fmt.Errorf("inspect: %w", err)
	}
	selectionSpec := Spec{Inspect: selections, Inputs: map[string]plugin.Input{}}
	for name := range request.Inputs {
		selectionSpec.Inputs[name] = plugin.Input{}
	}
	if err := selectionSpec.validateInspections(); err != nil {
		return plugin.Artifact{}, err
	}
	// Inspection selections are already concrete and must not be rendered again.
	delete(raw, "inspect")
	sources := map[string]*contents.Source{}
	defer closeSources(sources)
	facts := map[string]plugin.Subject{}
	for _, name := range slices.Sorted(maps.Keys(selections)) {
		selection := selections[name]
		source := sources[selection.Input]
		if source == nil {
			source, err = contents.Open(request.Inputs[selection.Input], request.Workspace)
			if err != nil {
				return plugin.Artifact{}, fmt.Errorf("inspect %s input %s: %w", name, selection.Input, err)
			}
			sources[selection.Input] = source
		}
		node, err := source.At(ctx, selection.Path)
		if err != nil {
			return plugin.Artifact{}, fmt.Errorf("inspect %s input %s path %q: %w", name, selection.Input, selection.Path, err)
		}
		subject, err := inspectNode(ctx, node, selection, filepath.Join(request.Workspace, "inspection-"+name))
		if err != nil {
			return plugin.Artifact{}, fmt.Errorf("inspect %s input %s path %q: %w", name, selection.Input, selection.Path, err)
		}
		facts[name] = subject
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
	data, err := json.Marshal(map[string]any{"env": environment, "facts": facts, "inputs": inputs})
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
	if len(selections) > 0 {
		object["inspect"] = selections
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
	if err := validatePreparedReferences(raw, spec); err != nil {
		return plugin.Artifact{}, err
	}
	if spec.Package.Filename == "" {
		spec.Package.Filename = artifactname.Filename(request.Identity.Name, spec.Package.Version, "", "pkg")
	}
	return build(ctx, spec, request.Inputs, request.Workspace, request.Timestamp, sources)
}

func validatePreparedReferences(raw map[string]any, spec Spec) error {
	for _, area := range []struct {
		name    string
		entries map[string]Entry
	}{{"payload", spec.Payload}, {"scripts", spec.Scripts}} {
		authored, _ := raw[area.name].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(area.entries)) {
			entry := area.entries[name]
			original, _ := authored[name].(map[string]any)
			input, _ := original["$input"].(string)
			if input != entry.Input {
				return fmt.Errorf("%s %q: $input references must be declared before preparation", area.name, name)
			}
		}
	}
	return nil
}

func inspectNode(ctx context.Context, node contents.Node, selection Inspection, work string) (plugin.Subject, error) {
	info, err := node.Stat()
	if err != nil {
		return plugin.Subject{}, err
	}
	if selection.Signature != nil {
		signer, err := signature.Parse(selection.Signature.Signer)
		if err != nil {
			return plugin.Subject{}, err
		}
		if signer.Scheme != signature.AppleDeveloperID {
			return plugin.Subject{}, errors.New("signature requires an Apple Developer ID team")
		}
		if info.IsDir() {
			_, err = apple.VerifyAppFS(ctx, node.FS, node.Path, signer)
		} else {
			var local string
			local, err = node.Materialize(ctx, work)
			if err == nil {
				_, err = apple.VerifyPackage(ctx, local, signer)
			}
		}
		if err != nil {
			return plugin.Subject{}, err
		}
	}
	observed, err := inspect.ReadFS(ctx, node.FS, node.Path)
	if err != nil {
		return plugin.Subject{}, err
	}
	return plugin.SelectSubject(observed, selection.Subject)
}
