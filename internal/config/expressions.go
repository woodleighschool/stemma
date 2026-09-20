package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/plugin"
)

func parseConfig(data []byte, value any) (map[string]any, error) {
	document, err := parseDocument(data, nil)
	if err != nil {
		return nil, err
	}
	if err := checkExpressions(document); err != nil {
		return nil, err
	}
	if document["kind"] == "Project" {
		if err := validateProjectDocument(document); err != nil {
			return nil, err
		}
		spec, _ := document["spec"].(map[string]any)
		destinations, _ := spec["destinations"].(map[string]any)
		for name, value := range destinations {
			destination, _ := value.(map[string]any)
			if err := evaluateEnvironment(destination, "config"); err != nil {
				return nil, fmt.Errorf("destination %s: %w", name, err)
			}
		}
		if err := evaluateEnvironment(spec, "plugins"); err != nil {
			return nil, fmt.Errorf("plugins: %w", err)
		}
		reconcile, _ := spec["reconcile"].(map[string]any)
		control, _ := reconcile["source_control"].(map[string]any)
		if err := evaluateEnvironment(control, "config"); err != nil {
			return nil, fmt.Errorf("reconcile source_control: %w", err)
		}
	}
	if err := decodeDocument(document, value); err != nil {
		return nil, err
	}
	return document, nil
}

func decodeDocument(document map[string]any, value any) error {
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func evaluateEnvironment(object map[string]any, key string) error {
	value, exists := object[key]
	if !exists {
		return nil
	}
	if err := expression.Check(value, "env"); err != nil {
		return err
	}
	resolved, err := expression.Eval(value, expression.Env())
	if err != nil {
		return err
	}
	object[key] = resolved
	return nil
}

func checkExpressions(document map[string]any) error {
	if err := expression.Check(document, "env", "facts", "evidence", "inputs"); err != nil {
		return err
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "suspend"} {
		if expression.Has(document[key]) {
			return fmt.Errorf("%s must be literal", key)
		}
	}
	spec, _ := document["spec"].(map[string]any)
	if expression.Has(spec["extends"]) {
		return fmt.Errorf("extends must be literal")
	}
	if document["kind"] != "Project" {
		return nil
	}
	if expression.Has(spec["imports"]) {
		return fmt.Errorf("imports must be literal")
	}
	components, _ := spec["components"].(map[string]any)
	for name, value := range components {
		component, _ := value.(map[string]any)
		if expression.Has(component["extends"]) {
			return fmt.Errorf("component %s: extends must be literal", name)
		}
	}
	destinations, _ := spec["destinations"].(map[string]any)
	for name, value := range destinations {
		destination, _ := value.(map[string]any)
		if expression.Has(destination["operation"]) {
			return fmt.Errorf("destination %s: operation must be literal", name)
		}
	}
	reconcile, _ := spec["reconcile"].(map[string]any)
	control, _ := reconcile["source_control"].(map[string]any)
	if expression.Has(control["type"]) {
		return fmt.Errorf("source_control.type must be literal")
	}
	return nil
}

func validateProjectDocument(document map[string]any) error {
	schema, err := baseSchema()
	if err != nil {
		return err
	}
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return plugin.ValidateSchema(schema, data)
}
