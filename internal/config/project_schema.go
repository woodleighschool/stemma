package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
	"oras.land/oras-go/v2/registry"
)

// LoadSchemaProject reads connection and plugin declarations without resolving
// credentials or loading the software documents the editor is about to author.
func LoadSchemaProject(filename string) (Project, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Project{}, err
	}
	var document ProjectDocument
	if _, err := parseDocument(data, &document); err != nil {
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
		image, err := expandEnvironment(provider.Image)
		if err != nil {
			return p, fmt.Errorf("plugin %s: %w", name, err)
		}
		provider.Image = image.(string)
		ref, err := registry.ParseReference(provider.Image)
		if err != nil || ref.Reference == "" {
			return p, fmt.Errorf("plugin %s: image must be an OCI registry reference with a tag or digest", name)
		}
		p.Plugins[name] = provider
	}
	return p, nil
}

// ProjectSchema binds named connections to their advertised operation contracts.
// Plugin schemas remain independent resources, including their local references.
func ProjectSchema(project Project, descriptor plugin.Descriptor) ([]byte, error) {
	data, err := Schema()
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
	connections, metadata := map[string]any{}, map[string]any{}
	installer := map[string]any{"type": "string", "pattern": namePattern.String(), "description": "Named output from this resource. Defaults to installer."}
	inputs := map[string]any{"type": "object", "propertyNames": map[string]any{"pattern": namePattern.String()}, "additionalProperties": installer}
	for name, connection := range project.Destinations {
		operation, ok := operations[connection.Operation]
		if !ok || operation.Kind != "reconcile" {
			return nil, fmt.Errorf("destination %s: unknown reconcile operation %q", name, connection.Operation)
		}
		metadataID := "https://stemma.invalid/editor/" + url.PathEscape(operation.Name) + "/metadata"
		settingsID := "https://stemma.invalid/editor/" + url.PathEscape(operation.Name) + "/config"
		metadataKey, settingsKey := "OperationMetadata_"+operation.Name, "OperationConfig_"+operation.Name
		if _, exists := definitions[metadataKey]; !exists {
			native, resources, err := editorResource(operation.MetadataSchema, metadataID)
			if err != nil {
				return nil, fmt.Errorf("operation %s metadata schema: %w", operation.Name, err)
			}
			walkEditorSchema(native, func(node map[string]any) { editorFacts(node, rootID+"#/$defs/FactReference") })
			addEditorInputs(native, resources, installer, inputs, map[string]bool{})
			definitions[metadataKey] = native
			settings, _, err := editorResource(operation.ConfigSchema, settingsID)
			if err != nil {
				return nil, fmt.Errorf("operation %s config schema: %w", operation.Name, err)
			}
			definitions[settingsKey] = settings
		}
		metadata[name] = map[string]any{"$ref": metadataID}
		connections[name] = map[string]any{"type": "object", "additionalProperties": false, "required": []string{"operation"}, "properties": map[string]any{"operation": map[string]any{"const": operation.Name}, "config": map[string]any{"$ref": settingsID}}}
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
		walkEditorSchema(native, func(node map[string]any) {
			fields, _ := node["properties"].(map[string]any)
			if _, ok := fields["destinations"]; ok {
				fields["destinations"] = map[string]any{"type": "object", "properties": metadata, "additionalProperties": false}
			}
		})
		definition := operation.Resource.Kind
		if operation.Resource.APIVersion != "stemma/v1alpha1" {
			definition = "Resource_" + operation.Name
		}
		if _, exists := definitions[definition]; !exists {
			schema["oneOf"] = append(schema["oneOf"].([]any), map[string]any{"$ref": "#/$defs/" + definition})
		}
		definitions[definition], err = resourceSchema(operation.Resource.APIVersion, operation.Resource.Kind, native)
		if err != nil {
			return nil, err
		}
	}
	definitions["ProjectSpec"].(map[string]any)["properties"].(map[string]any)["destinations"] = map[string]any{"type": "object", "properties": connections, "additionalProperties": false}
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

func editorFacts(node map[string]any, reference string) {
	wrap := func(value any) any { return map[string]any{"anyOf": []any{value, map[string]any{"$ref": reference}}} }
	for _, key := range []string{"properties", "patternProperties"} {
		properties, _ := node[key].(map[string]any)
		for name, property := range properties {
			properties[name] = wrap(property)
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := node[key].(map[string]any); ok {
			node[key] = wrap(child)
		}
	}
	if variants, ok := node["oneOf"].([]any); ok {
		if previous, exists := node["anyOf"]; exists {
			all, _ := node["allOf"].([]any)
			node["allOf"] = append(all, map[string]any{"anyOf": previous})
		}
		node["anyOf"] = variants
		delete(node, "oneOf")
	}
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
				resolved.Fragment = fragment
			}
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
