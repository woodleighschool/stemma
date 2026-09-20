package engine

import (
	"fmt"
	"strconv"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/expression"
)

func resourceDeclaration(resource config.Resource) (map[string]any, map[string]string, error) {
	raw := config.Merge(resource.Spec, nil)
	references := schedulingReferences(resource, raw)
	if err := literalReferences(references); err != nil {
		return nil, nil, err
	}
	if err := expression.Check(raw, "env", "facts", "evidence", "inputs"); err != nil {
		return nil, nil, err
	}
	destinations, hasDestinations := raw["destinations"]
	delete(raw, "destinations")
	var bindings map[string]string
	if deferredPreparation(resource) {
		for _, key := range []string{"inputs"} {
			if value, ok := raw[key]; ok {
				if err := expression.Check(value, "env"); err != nil {
					return nil, nil, fmt.Errorf("%s: %w", key, err)
				}
				resolved, err := expression.Eval(value, expression.Env())
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %w", key, err)
				}
				raw[key] = resolved
			}
		}
		preparation := config.Merge(raw, nil)
		delete(preparation, "inputs")
		if err := expression.Check(preparation, "env", "inputs"); err != nil {
			return nil, nil, err
		}
		var err error
		bindings, err = expression.Environment(preparation)
		if err != nil {
			return nil, nil, err
		}
	} else {
		if err := expression.Check(raw, "env"); err != nil {
			return nil, nil, err
		}
		resolved, err := expression.Eval(raw, expression.Env())
		if err != nil {
			return nil, nil, err
		}
		raw = resolved.(map[string]any)
	}
	if hasDestinations {
		raw["destinations"] = destinations
	}
	if config.Fingerprint(references) != config.Fingerprint(schedulingReferences(resource, raw)) {
		return nil, nil, fmt.Errorf("expressions cannot introduce or change scheduling references")
	}
	return raw, bindings, nil
}

func deferredPreparation(resource config.Resource) bool {
	return resource.APIVersion == "stemma/v1alpha1" && resource.Kind == "BuildMacPkg"
}

// Scheduling references are resolved before acquisition and remain literal.
func literalReferences(references map[string]any) error {
	for _, field := range sortedKeys(references) {
		if expression.Has(references[field]) {
			return fmt.Errorf("%s: reference must be literal", field)
		}
	}
	return nil
}

func schedulingReferences(resource config.Resource, spec map[string]any) map[string]any {
	references := map[string]any{}
	field := func(value any, key, location string) {
		object, _ := value.(map[string]any)
		if reference, exists := object[key]; exists {
			references[location+"["+strconv.Quote(key)+"]"] = reference
		}
	}
	input := func(value any, location string) {
		field(value, "resource", location)
		field(value, "resolver", location)
	}
	if resource.APIVersion == "stemma/v1alpha1" {
		switch resource.Kind {
		case "MacSoftware", "WindowsSoftware":
			input(spec["source"], "$[\"source\"]")
			if resource.Kind == "WindowsSoftware" {
				content, _ := spec["content"].(map[string]any)
				files, _ := content["files"].(map[string]any)
				for name, value := range files {
					input(value, "$[\"content\"][\"files\"]["+strconv.Quote(name)+"]")
				}
			}
		case "BuildMacPkg":
			inputs, _ := spec["inputs"].(map[string]any)
			for name, value := range inputs {
				input(value, "$[\"inputs\"]["+strconv.Quote(name)+"]")
			}
			for _, area := range []string{"payload", "scripts"} {
				entries, _ := spec[area].(map[string]any)
				for name, value := range entries {
					field(value, "$input", "$["+strconv.Quote(area)+"]["+strconv.Quote(name)+"]")
				}
			}
		}
	}
	// Publication references are reserved inside destination metadata, not inside
	// source settings, file entries, or a plugin's arbitrary configuration.
	var publications func(any, string)
	publications = func(value any, location string) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				path := location + "[" + strconv.Quote(key) + "]"
				if key == "resource" {
					references[path] = child
				} else {
					publications(child, path)
				}
			}
		case []any:
			for index, child := range value {
				publications(child, fmt.Sprintf("%s[%d]", location, index))
			}
		}
	}
	destinations, _ := spec["destinations"].(map[string]any)
	for name, value := range destinations {
		location := "$[\"destinations\"][" + strconv.Quote(name) + "]"
		field(value, "installer", location)
		field(value, "inputs", location)
		metadata, _ := value.(map[string]any)
		publications(destinationMetadata(metadata), location)
	}
	return references
}
