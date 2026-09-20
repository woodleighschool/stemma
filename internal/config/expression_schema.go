package config

import (
	"encoding/json"
	"maps"
	"slices"
)

// ExpressionSchema accepts authored expressions while retaining native constraints
// on literals. The evaluated value must still pass the original schema.
func ExpressionSchema(native json.RawMessage) (json.RawMessage, error) {
	if len(native) == 0 {
		return nil, nil
	}
	var schema any
	if err := json.Unmarshal(native, &schema); err != nil {
		return nil, err
	}
	return json.Marshal(expressionSchema(schema, true))
}

func expressionSchema(value any, whole bool) any {
	node, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if node["x-stemma-input"] == true {
		return node
	}
	for _, key := range []string{"$defs", "definitions", "properties", "patternProperties", "dependentSchemas"} {
		children, _ := node[key].(map[string]any)
		for name, child := range children {
			if key == "properties" && name == "$input" {
				if reference, ok := child.(map[string]any); ok {
					literalStringSchema(reference)
				}
				continue
			}
			children[name] = expressionSchema(child, key != "dependentSchemas")
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		children, _ := node[key].([]any)
		for i, child := range children {
			children[i] = expressionSchema(child, key == "prefixItems")
		}
	}
	if node["type"] == "object" || node["properties"] != nil || node["additionalProperties"] != nil {
		literal := map[string]any{"not": map[string]any{"pattern": `\{\{`}}
		if names, exists := node["propertyNames"]; exists {
			node["propertyNames"] = map[string]any{"allOf": []any{names, literal}}
		} else {
			node["propertyNames"] = literal
		}
	}
	for _, key := range []string{"not", "if", "then", "else", "items", "contains", "additionalProperties", "unevaluatedProperties", "contentSchema"} {
		if child, exists := node[key]; exists {
			if _, schema := child.(map[string]any); schema {
				node[key] = expressionSchema(child, key == "items" || key == "contains" || key == "additionalProperties" || key == "unevaluatedProperties")
			}
		}
	}
	if !whole {
		return node
	}
	literal := maps.Clone(node)
	clear(node)
	// IDs and definitions retain their location so local references keep their scope.
	for _, key := range []string{"$id", "$schema", "$defs", "definitions", "$anchor", "$dynamicAnchor", "description", "default", "title", "writeOnly", "readOnly", "examples"} {
		if value, exists := literal[key]; exists {
			node[key] = value
			delete(literal, key)
		}
	}
	pattern := wholeExpressionSchema()
	if acceptsString(literal) {
		pattern = map[string]any{"type": "string", "pattern": `\{\{[\s\S]+\}\}`}
	}
	node["anyOf"] = []any{literal, pattern}
	return node
}

func literalStringSchema(node map[string]any) {
	if node["type"] == "string" {
		node["not"] = map[string]any{"pattern": `\{\{`}
	}
}

func wholeExpressionSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": `^\{\{[\s\S]+\}\}$`}
}

func acceptsString(node map[string]any) bool {
	if node["type"] == "string" {
		return true
	}
	if types, ok := node["type"].([]any); ok {
		return slices.Contains(types, any("string"))
	}
	return false
}
