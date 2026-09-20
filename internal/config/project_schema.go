package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// LoadSchemaProject reads connection and plugin declarations without resolving
// credentials or loading the software documents the editor is about to write.
func LoadSchemaProject(filename string) (Project, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Project{}, err
	}
	raw, err := parseDocument(data, nil)
	if err != nil {
		return Project{}, err
	}
	if err := checkExpressions(raw); err != nil {
		return Project{}, err
	}
	if err := validateProjectDocument(raw); err != nil {
		return Project{}, err
	}
	spec, _ := raw["spec"].(map[string]any)
	if err := evaluateEnvironment(spec, "plugins"); err != nil {
		return Project{}, fmt.Errorf("plugins: %w", err)
	}
	var document ProjectDocument
	if err := decodeDocument(raw, &document); err != nil {
		return Project{}, err
	}
	if err := validateHeader(document.APIVersion, document.Kind, "Project", document.Metadata); err != nil {
		return Project{}, err
	}
	p := Project{Project: document.Metadata.Name, Destinations: document.Spec.Destinations, Plugins: document.Spec.Plugins}
	for name, destination := range p.Destinations {
		if !namePattern.MatchString(name) {
			return p, fmt.Errorf("invalid destination name %q", name)
		}
		if err := validateOperation(destination.Operation); err != nil {
			return p, fmt.Errorf("destination %s: %w", name, err)
		}
	}
	for name, provider := range p.Plugins {
		if !namePattern.MatchString(name) || !provider.Trusted {
			return p, fmt.Errorf("plugin %s: requires a valid name and trusted: true", name)
		}
		if err := provider.Validate(); err != nil {
			return p, fmt.Errorf("plugin %s: %w", name, err)
		}

		p.Plugins[name] = provider
	}
	return p, nil
}

// ProjectSchema binds named connections to their advertised operation contracts.
// Plugin schemas remain independent resources, including their local references.
func ProjectSchema(project Project, descriptor plugin.Descriptor) ([]byte, error) {
	return catalogSchema(&project, descriptor)
}

// Schema describes registered operations without binding destination names to a project.
func Schema(descriptor plugin.Descriptor) ([]byte, error) {
	return catalogSchema(nil, descriptor)
}

func catalogSchema(project *Project, descriptor plugin.Descriptor) ([]byte, error) {
	data, err := baseSchema()
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}
	definitions := schema["$defs"].(map[string]any)
	rootID := schema["$id"].(string)
	operations := map[string]plugin.Operation{}
	for _, operation := range descriptor.Operations {
		operations[operation.Name] = operation
	}
	input, err := catalogInputSchema(descriptor)
	if err != nil {
		return nil, err
	}
	definitions["Input"] = input
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	partialInput, _, err := editorResource(encodedInput, "https://stemma.invalid/editor/partial-input")
	if err != nil {
		return nil, err
	}
	walkEditorSchema(partialInput, func(node map[string]any) {
		delete(node, "required")
		if alternatives, ok := node["oneOf"]; ok {
			node["anyOf"] = alternatives
			delete(node, "oneOf")
		}
	}, "if", "not")
	definitions["PartialInput"] = partialInput
	var connections []any
	var nativeMetadata []any
	metadata := map[string]any{}
	installer := map[string]any{"type": "string", "pattern": namePattern.String(), "description": "Named output from this resource. Defaults to installer."}
	inputs := map[string]any{"type": "object", "propertyNames": map[string]any{"pattern": namePattern.String()}, "additionalProperties": installer}
	for _, operation := range descriptor.Operations {
		if operation.Kind != "reconcile" {
			continue
		}
		metadataID := "https://stemma.invalid/editor/" + url.PathEscape(operation.Name) + "/metadata"
		settingsID := "https://stemma.invalid/editor/" + url.PathEscape(operation.Name) + "/config"
		metadataKey, settingsKey := "OperationMetadata_"+operation.Name, "OperationConfig_"+operation.Name
		nativeMetadata = append(nativeMetadata, map[string]any{"$ref": metadataID})
		if _, exists := definitions[metadataKey]; !exists {
			native, resources, err := editorResource(operation.MetadataSchema, metadataID)
			if err != nil {
				return nil, fmt.Errorf("operation %s metadata schema: %w", operation.Name, err)
			}
			expressionSchema(native, false)
			addEditorInputs(native, resources, installer, inputs, map[string]bool{})
			definitions[metadataKey] = native
			settings, _, err := editorResource(operation.ConfigSchema, settingsID)
			if err != nil {
				return nil, fmt.Errorf("operation %s config schema: %w", operation.Name, err)
			}
			expressionSchema(settings, false)
			definitions[settingsKey] = settings
		}
		required := []string{"operation"}
		if len(operation.ConfigSchema) > 0 && plugin.ValidateSchema(operation.ConfigSchema, []byte(`{}`)) != nil {
			required = append(required, "config")
		}
		connections = append(connections, map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": map[string]any{
			"operation": map[string]any{"const": operation.Name, "description": "Registered destination operation."},
			"config":    map[string]any{"$ref": settingsID, "description": "Connection settings for this destination. Credentials may reference environment variables."},
		}})
	}
	destinations := map[string]any{"type": "object", "description": "Native metadata for each named Project destination. Explicit values override derived values.", "properties": metadata, "additionalProperties": false}
	if project == nil {
		destinations["propertyNames"] = map[string]any{"pattern": namePattern.String()}
		if len(nativeMetadata) > 0 {
			destinations["additionalProperties"] = map[string]any{"anyOf": nativeMetadata}
		}
	} else {
		for name, connection := range project.Destinations {
			operation, ok := operations[connection.Operation]
			if !ok || operation.Kind != "reconcile" {
				return nil, fmt.Errorf("destination %s: unknown reconcile operation %q", name, connection.Operation)
			}
			metadata[name] = map[string]any{"$ref": "https://stemma.invalid/editor/" + url.PathEscape(operation.Name) + "/metadata"}
		}
	}
	for _, operation := range descriptor.Operations {
		if operation.Resource == nil {
			continue
		}
		specID := "https://stemma.invalid/editor/" + url.PathEscape(operation.Name) + "/spec"
		native, _, err := editorResource(operation.ConfigSchema, specID)
		if err != nil {
			return nil, err
		}
		expressionSchema(native, false)
		walkEditorSchema(native, func(node map[string]any) {
			if node["x-stemma-input"] == true {
				description := node["description"]
				clear(node)
				if description != nil {
					node["description"] = description
				}
				node["$ref"] = rootID + "#/$defs/Input"
				return
			}
			fields, _ := node["properties"].(map[string]any)
			if _, ok := fields["destinations"]; ok {
				fields["destinations"] = destinations
			}
		})
		definition := operation.Resource.Kind
		if operation.Resource.APIVersion != "stemma/v1alpha1" {
			definition = "Resource_" + operation.Name
		}
		schema["oneOf"] = append(schema["oneOf"].([]any), map[string]any{"$ref": "#/$defs/" + definition})
		definitions[definition], err = resourceSchema(operation.Resource.APIVersion, operation.Resource.Kind, native)
		if err != nil {
			return nil, err
		}
	}
	var connectionSchema any = false
	if len(connections) > 0 {
		connectionSchema = map[string]any{"oneOf": connections}
	}
	definitions["ProjectSpec"].(map[string]any)["properties"].(map[string]any)["destinations"] = map[string]any{"type": "object", "description": "Named destination connections. Resource destination names refer to these entries.", "propertyNames": map[string]any{"pattern": namePattern.String()}, "additionalProperties": connectionSchema}
	data, err = json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Walk only schema positions: examples, defaults and enum values are user data.
func walkEditorSchema(value any, visit func(map[string]any), preserve ...string) {
	node, ok := value.(map[string]any)
	if !ok {
		return
	}
	for _, child := range editorChildren(node, preserve...) {
		walkEditorSchema(child, visit, preserve...)
	}
	visit(node)
}

func editorChildren(node map[string]any, preserve ...string) []map[string]any {
	var result []map[string]any
	add := func(value any) {
		if child, ok := value.(map[string]any); ok {
			result = append(result, child)
		}
	}
	for _, key := range []string{"$defs", "definitions", "properties", "patternProperties", "dependentSchemas"} {
		children, _ := node[key].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(children)) {
			add(children[name])
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		children, _ := node[key].([]any)
		for _, child := range children {
			add(child)
		}
	}
	for _, key := range []string{"not", "if", "then", "else", "items", "contains", "additionalProperties", "unevaluatedProperties", "propertyNames", "contentSchema"} {
		if !slices.Contains(preserve, key) {
			add(node[key])
		}
	}
	return result
}

func editorResource(data json.RawMessage, identity string) (map[string]any, map[string]map[string]any, error) {
	var value any = map[string]any{"type": "object"}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, nil, err
		}
	}
	root, ok := value.(map[string]any)
	if !ok {
		root = map[string]any{"allOf": []any{value}}
	}
	// Rebase resource IDs as well as refs so two plugins may use the same $defs
	// names or original $id without changing one another's resolution scope.
	original, _ := url.Parse(identity)
	if id, ok := root["$id"].(string); ok {
		parsed, err := url.Parse(id)
		if err != nil {
			return nil, nil, err
		}
		original = original.ResolveReference(parsed)
	}
	type scopedNode struct {
		node map[string]any
		base *url.URL
	}
	var nodes []scopedNode
	resources := map[string]map[string]any{identity: root}
	ids := map[string]string{original.String(): identity}
	var invalidID error
	var collect func(map[string]any, *url.URL, bool)
	collect = func(node map[string]any, base *url.URL, first bool) {
		if id, exists := node["$id"].(string); exists && !first {
			parsed, err := url.Parse(id)
			if err != nil {
				invalidID = err
				return
			}
			base = base.ResolveReference(parsed)
			rebased := fmt.Sprintf("%s/resource/%d", identity, len(resources))
			ids[base.String()] = rebased
			resources[rebased] = node
			node["$id"] = rebased
		}
		nodes = append(nodes, scopedNode{node, base})
		for _, child := range editorChildren(node) {
			collect(child, base, false)
		}
	}
	collect(root, original, true)
	if invalidID != nil {
		return nil, nil, invalidID
	}
	for _, scoped := range nodes {
		for _, key := range []string{"$ref", "$dynamicRef"} {
			ref, ok := scoped.node[key].(string)
			if !ok {
				continue
			}
			parsed, err := url.Parse(ref)
			if err != nil {
				return nil, nil, err
			}
			resolved := scoped.base.ResolveReference(parsed)
			fragment := resolved.Fragment
			resolved.Fragment = ""
			if rebased, exists := ids[resolved.String()]; exists {
				resolved, _ = url.Parse(rebased)
			}
			resolved.Fragment = fragment
			scoped.node[key] = resolved.String()
		}
	}
	root["$id"] = identity
	return root, resources, nil
}

func addEditorInputs(node map[string]any, resources map[string]map[string]any, installer, inputs any, seen map[string]bool) {
	editEditorObject(node, resources, seen, func(node map[string]any) {
		properties, _ := node["properties"].(map[string]any)
		if properties == nil {
			properties = map[string]any{}
			node["properties"] = properties
		}
		properties["installer"], properties["inputs"] = installer, inputs
	})
}

func editEditorObject(node map[string]any, resources map[string]map[string]any, seen map[string]bool, visit func(map[string]any)) {
	visit(node)
	if ref, ok := node["$ref"].(string); ok && !seen[ref] {
		seen[ref] = true
		parsed, err := url.Parse(ref)
		if err == nil {
			fragment := parsed.Fragment
			parsed.Fragment = ""
			var target any = resources[parsed.String()]
			if path, ok := strings.CutPrefix(fragment, "/"); ok {
				for segment := range strings.SplitSeq(path, "/") {
					object, _ := target.(map[string]any)
					target = object[strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")]
				}
			} else if fragment != "" {
				target = nil
				walkEditorSchema(resources[parsed.String()], func(child map[string]any) {
					if child["$anchor"] == fragment {
						target = child
					}
				})
			}
			if target, ok := target.(map[string]any); ok && target != nil {
				editEditorObject(target, resources, seen, visit)
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		children, _ := node[key].([]any)
		for _, child := range children {
			if child, ok := child.(map[string]any); ok {
				editEditorObject(child, resources, seen, visit)
			}
		}
	}
}

// catalogInputSchema closes the dynamic input slots using the same contracts
// advertised by the loaded resolvers. No vendor fields are copied here.
func catalogInputSchema(descriptor plugin.Descriptor) (map[string]any, error) {
	data, err := json.Marshal(source.InputSchema(reflect.TypeFor[plugin.Input]()))
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}
	variants, _ := schema["oneOf"].([]any)
	walkEditorSchema(variants[0], literalStringSchema)
	for _, variant := range variants[1:] {
		if node, ok := variant.(map[string]any); ok {
			properties, _ := node["properties"].(map[string]any)
			resolver := properties["resolver"]
			delete(properties, "resolver")
			expressionSchema(node, false)
			if resolver != nil {
				properties["resolver"] = resolver
			}
		}
	}
	for _, operation := range descriptor.Operations {
		if operation.Resolver == nil {
			continue
		}
		identity := "https://stemma.invalid/editor/" + url.PathEscape(operation.Name) + "/input"
		config, resources, err := editorResource(operation.ConfigSchema, identity)
		if err != nil {
			return nil, err
		}
		expressionSchema(config, false)
		editEditorObject(config, resources, map[string]bool{}, func(node map[string]any) {
			properties, _ := node["properties"].(map[string]any)
			if properties == nil {
				properties = map[string]any{}
				node["properties"] = properties
			}
			properties["resolver"] = map[string]any{"const": operation.Name, "description": "Loaded resolver selecting this input."}
		})
		required, _ := config["required"].([]any)
		config["required"] = append(required, "resolver")
		schema["oneOf"] = append(schema["oneOf"].([]any), config)
	}
	return schema, nil
}
