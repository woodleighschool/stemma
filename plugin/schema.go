package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	defaults "github.com/kaptinlin/jsonschema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateSchema validates data against a self-contained JSON Schema. Schemas
// default to draft 2020-12. External network and filesystem resources are disabled.
func ValidateSchema(schema, data json.RawMessage) error {
	compiled, err := compileSchema(schema)
	if err != nil {
		return err
	}
	return validateData(compiled, data)
}

func compileSchema(schema json.RawMessage) (*jsonschema.Schema, error) {
	if len(schema) > messageLimit {
		return nil, errors.New("schema exceeds size limit")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("schema JSON: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noResources{})
	const resource = "https://stemma.invalid/operation.schema.json"
	if err := compiler.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return compiled, nil
}

func validateData(schema *jsonschema.Schema, data json.RawMessage) error {
	if len(data) > messageLimit {
		return errors.New("data exceeds size limit")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("data JSON: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("contract: %w", err)
	}
	return nil
}

// defaultData uses the schema library's decoder to fill omitted properties.
// Decode into a JSON object so explicit zero values retain their meaning.
func defaultData(schema *defaults.Schema, data json.RawMessage) (json.RawMessage, error) {
	var value map[string]any
	if err := schema.Unmarshal(&value, []byte(data)); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func compileDefaults(data json.RawMessage) (*defaults.Schema, error) {
	compiler := defaults.NewCompiler()
	clear(compiler.Loaders)
	return compiler.Compile(data)
}

type noResources struct{}

func (noResources) Load(string) (any, error) {
	return nil, errors.New("external schema resources are not allowed")
}
