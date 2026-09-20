package config

import (
	"encoding/json"
	"net/url"

	"github.com/invopop/jsonschema"
)

// catalogSchemaID scopes references inside the bundled document independently of its published URL.
const catalogSchemaID = "https://stemma.invalid/catalog.schema.json"

// baseSchema describes the static project envelope. Resource kinds come from the registry.
func baseSchema() ([]byte, error) {
	r := jsonschema.Reflector{}
	raw, err := json.Marshal(r.Reflect(ProjectDocument{}))
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	schema["$id"] = catalogSchemaID
	schema["title"] = "Stemma documents"
	schema["description"] = "A Project imports family files containing resource documents separated by ---. Each kind owns preparation; destination operations own native publication fields."
	delete(schema, "$ref")
	definitions := schema["$defs"].(map[string]any)
	definitions["Metadata"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)["pattern"] = namePattern.String()
	expressionSchema(definitions["Plugin"], false)
	imports := definitions["ProjectSpec"].(map[string]any)["properties"].(map[string]any)["imports"].(map[string]any)
	literalStringSchema(imports["items"].(map[string]any))
	for _, name := range []string{"Destination", "SourceControl"} {
		properties := definitions[name].(map[string]any)["properties"].(map[string]any)
		properties["config"] = expressionSchema(properties["config"], true)
	}
	definitions["Plugin"].(map[string]any)["oneOf"] = []any{
		map[string]any{"required": []string{"image"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"path"}}, map[string]any{"required": []string{"entrypoint"}}}}},
		map[string]any{"required": []string{"path"}, "not": map[string]any{"required": []string{"image"}}},
	}
	variants := []any{map[string]any{"$ref": "#/$defs/ProjectDocument"}}
	schema["oneOf"] = variants
	data, err := json.MarshalIndent(schema, "", "  ")
	return append(data, '\n'), err
}

func resourceSchema(version, kind string, spec map[string]any) (map[string]any, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	identity := "https://stemma.invalid/editor/resource/" + url.PathEscape(version+"/"+kind)
	complete, _, err := editorResource(data, identity+"/complete")
	if err != nil {
		return nil, err
	}
	partial, resources, err := editorResource(data, identity+"/partial")
	if err != nil {
		return nil, err
	}
	// Inherited specs supply partial overrides. Validate each supplied value;
	// the registered kind validates the fully composed object at runtime.
	walkEditorSchema(partial, func(node map[string]any) {
		if node["$ref"] == catalogSchemaID+"#/$defs/Input" {
			node["$ref"] = catalogSchemaID + "#/$defs/PartialInput"
		}
		delete(node, "required")
		if variants, ok := node["oneOf"]; ok {
			all, _ := node["allOf"].([]any)
			node["allOf"] = append(all, map[string]any{"anyOf": variants})
			delete(node, "oneOf")
		}
	}, "not", "if")
	editEditorObject(partial, resources, map[string]bool{}, func(node map[string]any) {
		properties, _ := node["properties"].(map[string]any)
		if properties == nil {
			properties = map[string]any{}
			node["properties"] = properties
		}
		properties["extends"] = map[string]any{"type": "string", "pattern": namePattern.String()}
	})
	specSchema := map[string]any{"anyOf": []any{complete, map[string]any{"allOf": []any{partial, map[string]any{"required": []string{"extends"}}}}}}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"apiVersion", "kind", "metadata", "spec"}, "properties": map[string]any{
		"apiVersion": map[string]any{"const": version}, "kind": map[string]any{"const": kind},
		"metadata": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string", "pattern": namePattern.String()}}},
		"suspend":  map[string]any{"type": "boolean", "description": "Keep the resource out of every run that does not select it. It still validates and keeps its reviewed lock entries; selecting it, or a suspended resource consuming its outputs, runs it."},
		"spec":     specSchema,
	}}, nil
}
