package engine

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/plugin"
)

func resolveMetadata(software plugin.ResourceResult, native map[string]any, facts plugin.Facts, evidence map[string]json.RawMessage) (map[string]any, map[string]string, error) {
	roots, err := expression.Roots(native)
	if err != nil {
		return nil, nil, err
	}
	selected := map[string]plugin.Subject{}
	for _, subject := range facts.Subjects {
		selected[subject.ID] = subject
	}
	if slices.Contains(roots, "facts") {
		for _, name := range sortedKeys(software.Subjects) {
			subject, err := plugin.SelectSubject(facts, software.Subjects[name])
			if err != nil {
				return nil, nil, fmt.Errorf("subject %s: %w", name, err)
			}
			selected[name] = subject
		}
	}
	// Project the typed contract into data, preserving JSON field presence and
	// excluding Go methods from the evaluator's environment.
	data, err := json.Marshal(map[string]any{"facts": selected, "evidence": evidence})
	if err != nil {
		return nil, nil, err
	}
	contexts := expression.Env()
	if err := json.Unmarshal(data, &contexts); err != nil {
		return nil, nil, err
	}
	if evidence == nil {
		contexts["evidence"] = map[string]any{}
	}
	resolved, err := expression.Eval(native, contexts)
	if err != nil {
		return nil, nil, err
	}
	metadata := resolved.(map[string]any)
	authoredRefs, err := publicationReferences(native)
	if err != nil {
		return nil, nil, err
	}
	resolvedRefs, err := publicationReferences(metadata)
	if err != nil {
		return nil, nil, err
	}
	keys := func(refs []plugin.ResourceReference) []string {
		result := make([]string, 0, len(refs))
		for _, reference := range refs {
			result = append(result, reference.Key())
		}
		slices.Sort(result)
		return slices.Compact(result)
	}
	if !slices.Equal(keys(authoredRefs), keys(resolvedRefs)) {
		return nil, nil, fmt.Errorf("metadata expressions must preserve declared resource references")
	}
	origins := map[string]string{}
	var visit func(any, any, string, string)
	visit = func(authored, resolved any, path, inherited string) {
		fields, authoredObject := authored.(map[string]any)
		if !authoredObject && expression.Has(authored) {
			inherited = "expression"
		}
		if values, ok := resolved.(map[string]any); ok && len(values) > 0 {
			for key, value := range values {
				name := key
				if path != "" {
					name = path + "." + key
				}
				visit(fields[key], value, name, inherited)
			}
			return
		}
		if path != "" {
			if inherited == "" {
				inherited = "explicit"
			}
			origins[path] = inherited
		}
	}
	visit(native, metadata, "", "")
	return metadata, origins, nil
}

func destinationMetadata(input map[string]any) map[string]any {
	result := config.Merge(input, nil)
	delete(result, "installer")
	delete(result, "inputs")
	return result
}
