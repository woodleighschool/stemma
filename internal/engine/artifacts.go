package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
)

func derivePackage(ctx context.Context, store *cas.Store, ops *operations, input Prepared, spec config.Artifact, work string) (Prepared, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return Prepared{}, err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return Prepared{}, err
	}
	step, err := runStep(ctx, store, ops, config.Step{Name: "package", Operation: "pkg", Config: settings}, map[string]Prepared{"input": input}, input.Source, work)
	if err != nil {
		return Prepared{}, err
	}
	result, exists := step.Artifacts["artifact"]
	if !exists {
		return Prepared{}, fmt.Errorf("pkg did not produce artifact")
	}
	result.Cached = step.Cached
	return result, nil
}

func destinationMetadata(input map[string]any) map[string]any {
	result := config.Merge(input, nil)
	delete(result, "artifact")
	delete(result, "inputs")
	return result
}

func prepareSoftware(ctx context.Context, store *cas.Store, ops *operations, software config.Software, prepared *Prepared, work string, all bool, report *SoftwareReport) (map[string]Prepared, error) {
	outputs := map[string]Prepared{}
	var entry source.Entry
	if prepared != nil {
		outputs["prepared"], entry = *prepared, prepared.Source
		original := Prepared{Source: entry, Payload: entry.Artifact, Filename: entry.Filename, Tree: entry.Tree}
		original, err := materialize(ctx, store, original, filepath.Join(work, "original"))
		if err != nil {
			return outputs, err
		}
		facts, err := inspect(ctx, original.Path)
		if err != nil {
			return outputs, err
		}
		original.Format, original.Version, original.Facts = facts.Format, facts.Version, facts.Facts
		original.Evidence = prepared.Evidence
		outputs["source"] = original
	}
	report.Artifacts = map[string]Prepared{}
	for _, name := range sortedKeys(software.Artifacts) {
		used := all || software.Verification.Subject == "artifacts/"+name
		for _, metadata := range software.Destinations {
			if metadata["artifact"] == "artifacts/"+name {
				used = true
			}
			for _, reference := range destinationReferences(metadata) {
				if reference == "artifacts/"+name {
					used = true
				}
			}
		}
		for _, step := range software.Steps {
			for _, ref := range step.Inputs {
				if ref == "artifacts/"+name {
					used = true
				}
			}
		}
		if !used {
			continue
		}
		if prepared == nil {
			return outputs, fmt.Errorf("artifact %s requires a prepared source", name)
		}
		artifact, err := derivePackage(ctx, store, ops, *prepared, software.Artifacts[name], filepath.Join(work, "artifacts", name))
		if err != nil {
			report.ArtifactErrors = map[string]string{name: err.Error()}
			return outputs, fmt.Errorf("artifact %s: %w", name, err)
		}
		outputs["artifacts/"+name], report.Artifacts[name] = artifact, artifact
	}
	for _, step := range software.Steps {
		inputs := map[string]Prepared{}
		for name, reference := range step.Inputs {
			input, exists := outputs[reference]
			if !exists {
				return outputs, fmt.Errorf("step %s input %s: missing output %s", step.Name, name, reference)
			}
			inputs[name] = input
		}
		if hasFactReference(step.Config) {
			if len(inputs) != 1 {
				return outputs, fmt.Errorf("step %s: fact references require one unambiguous input", step.Name)
			}
			for name, input := range inputs {
				if !input.SuppliedFacts {
					facts, err := Inspect(ctx, input.Path)
					if err != nil {
						return outputs, err
					}
					input.Facts = facts.Facts
					inputs[name] = input
				}
				resolved, _, err := resolveMetadata(software, step.Config, input.Facts, "")
				if err != nil {
					return outputs, fmt.Errorf("step %s: %w", step.Name, err)
				}
				step.Config = resolved
			}
		}
		result, err := runStep(ctx, store, ops, step, inputs, entry, filepath.Join(work, "steps", step.Name))
		report.Steps = append(report.Steps, result)
		if err != nil {
			return outputs, fmt.Errorf("step %s: %w", step.Name, err)
		}
		for name, artifact := range result.Artifacts {
			outputs[step.Name+"/"+name] = artifact
		}
	}
	if !requested(software.Verification) && software.Verification.Subject == "" {
		return outputs, nil
	}
	subjects := map[string]bool{}
	if subject := software.Verification.Subject; subject == "" || subject == "payload" {
		for _, metadata := range software.Destinations {
			subject, _ := metadata["artifact"].(string)
			if subject == "" {
				subject = "prepared"
			}
			subjects[subject] = true
		}
		if len(subjects) == 0 {
			subjects["prepared"] = true
		}
	} else if subject != "source" {
		subjects[subject] = true
	}
	for _, subject := range sortedKeys(subjects) {
		artifact, exists := outputs[subject]
		if !exists {
			return outputs, fmt.Errorf("verification subject references missing output %s", subject)
		}
		if requested(software.Verification) {
			evidence, err := verify(artifact.Path, software.Verification)
			artifact.Evidence = &evidence
			outputs[subject] = artifact
			if subject == "prepared" {
				report.Prepared = &artifact
			} else {
				owner, name, _ := strings.Cut(subject, "/")
				if owner == "artifacts" {
					report.Artifacts[name] = artifact
				} else {
					for i := range report.Steps {
						if report.Steps[i].Name == owner {
							report.Steps[i].Artifacts[name] = artifact
							break
						}
					}
				}
			}
			if err != nil {
				return outputs, fmt.Errorf("verification subject %s: %w", subject, err)
			}
		}
	}
	return outputs, nil
}
