package config

import (
	"encoding/json"
	"net/url"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/internal/macpkg"
	"github.com/woodleighschool/stemma/internal/macsoftware"
	"github.com/woodleighschool/stemma/internal/windowssoftware"
)

// Schema describes the initial resource contracts and the project envelope.
func Schema() ([]byte, error) {
	r := jsonschema.Reflector{}
	raw, err := json.Marshal(r.Reflect(ProjectDocument{}))
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	schema["$id"] = "https://raw.githubusercontent.com/woodleighschool/stemma/main/stemma.schema.json"
	schema["title"] = "Stemma documents"
	schema["description"] = "A Project imports family files containing resource documents separated by ---. Each kind owns preparation; destination operations own native publication fields."
	delete(schema, "$ref")
	definitions := schema["$defs"].(map[string]any)
	variants := []any{map[string]any{"$ref": "#/$defs/ProjectDocument"}}
	for _, item := range []struct {
		kind string
		spec any
	}{{"BuildMacPkg", macpkg.Spec{}}, {"MacSoftware", macsoftware.Spec{}}, {"WindowsSoftware", windowssoftware.Spec{}}} {
		reflector := jsonschema.Reflector{DoNotReference: true}
		raw, err := json.Marshal(reflector.Reflect(item.spec))
		if err != nil {
			return nil, err
		}
		var spec map[string]any
		if err := json.Unmarshal(raw, &spec); err != nil {
			return nil, err
		}
		definitions[item.kind], err = resourceSchema("stemma/v1alpha1", item.kind, spec)
		if err != nil {
			return nil, err
		}
		variants = append(variants, map[string]any{"$ref": "#/$defs/" + item.kind})
	}
	definitions["FactReference"] = map[string]any{"type": "object", "required": []string{"$fact"}, "additionalProperties": false, "properties": map[string]any{"$fact": map[string]any{"type": "string"}}}
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
	authored := map[string]any{"anyOf": []any{complete, map[string]any{"allOf": []any{partial, map[string]any{"required": []string{"extends"}}}}}}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"apiVersion", "kind", "metadata", "spec"}, "properties": map[string]any{
		"apiVersion": map[string]any{"const": version}, "kind": map[string]any{"const": kind},
		"metadata": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string", "pattern": namePattern.String()}}}, "spec": authored,
	}}, nil
}
