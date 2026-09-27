package macpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/artifactname"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/expression"
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
	var verified []byte
	if spec.Signature != nil {
		result, err := verifyInput(ctx, sources, *spec.Signature, request.Derive == "signature")
		if err != nil {
			return plugin.Artifact{}, err
		}
		if verified, err = json.Marshal(result); err != nil {
			return plugin.Artifact{}, err
		}
	}
	artifact, err := build(ctx, spec, sources, request.Workspace)
	if err != nil || verified == nil {
		return artifact, err
	}
	// Intune and stemma signature read "signature" as proof about the
	// published package, which the builder never signs.
	artifact.Evidence = map[string]json.RawMessage{"input.signature": verified}
	return artifact, nil
}

// verifyInput checks the publisher signature of the input a build wraps: a
// PKG's package signature, or every application in a disk image, archive or
// folder outside another application. Deriving reports the observed signer
// when none is declared.
func verifyInput(ctx context.Context, sources *sources, policy InputSignature, derive bool) (signature.InputResult, error) {
	var want signature.Signer
	switch {
	case policy.Signer != "":
		var err error
		if want, err = signature.Parse(policy.Signer); err != nil {
			return signature.InputResult{}, err
		}
	case !derive:
		return signature.InputResult{}, errors.New("signature.signer is required; derive it with stemma signature")
	}
	source, err := sources.get(policy.Input)
	if err != nil {
		return signature.InputResult{}, err
	}
	facts, err := sources.inventory(ctx, policy.Input)
	if err != nil {
		return signature.InputResult{}, fmt.Errorf("input %q: %w", policy.Input, err)
	}
	done := plugin.Stage(ctx, "Verifying input signature", plugin.Detail(policy.Input))
	result, err := verifySource(ctx, source, facts, want)
	done(err, plugin.Detail(result.Name))
	if err != nil {
		return signature.InputResult{}, fmt.Errorf("input %q: %w", policy.Input, err)
	}
	return signature.InputResult{Input: policy.Input, Result: result}, nil
}

func verifySource(ctx context.Context, source *contents.Source, facts plugin.Facts, want signature.Signer) (signature.Result, error) {
	local := source.Artifact().Path
	if !source.Traversable() {
		return apple.VerifyPackage(ctx, local, want)
	}
	apps := map[string]bool{}
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps[subject.ID] = true
		}
	}
	var top []string
	for _, subject := range facts.Subjects {
		if subject.App == nil || apps[subject.Parent] {
			continue
		}
		if subject.ID == "." {
			return apple.VerifyApp(ctx, local, want)
		}
		top = append(top, subject.Path)
	}
	root, err := source.At(ctx, ".")
	if err != nil {
		return signature.Result{}, err
	}
	return apple.VerifyAppsFS(ctx, root.FS, top, want)
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
