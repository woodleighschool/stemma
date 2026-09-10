package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func subjectSelectors(software config.Software) map[string]plugin.SubjectSelector {
	result := make(map[string]plugin.SubjectSelector, len(software.Subjects))
	for name, selector := range software.Subjects {
		result[name] = plugin.SubjectSelector(selector)
	}
	return result
}

func peerBindings(ops *operations, project config.Project, destination string, current *state) map[string]json.RawMessage {
	result := map[string]json.RawMessage{}
	connection := ops.fingerprint(project.Destinations[destination])
	for software, spec := range project.Software {
		if _, present := spec.Destinations[destination]; !present {
			continue
		}
		value := current.Bindings[software+"/"+destination]
		if value.Connection == connection && len(value.Binding) > 0 && string(value.Binding) != "null" {
			result[software] = value.Binding
		}
	}
	return result
}

type destinationRef struct {
	Software    string
	Destination string
}

// Providers declare software references on their own connection during validation.
func orderDestinations(ctx context.Context, project config.Project, ops *operations, root string, selected []string) ([]destinationRef, map[destinationRef][]destinationRef, error) {
	dependencies := map[destinationRef][]destinationRef{}
	var nodes []destinationRef
	for _, name := range selected {
		software := project.Software[name]
		for _, destination := range sortedKeys(software.Destinations) {
			node := destinationRef{name, destination}
			nodes = append(nodes, node)
			connection := project.Destinations[destination]
			metadata, _ := json.Marshal(staticMetadata(destinationMetadata(software.Destinations[destination])))
			settings, _ := json.Marshal(connection.Config)
			request := plugin.ReconcileRequest{Method: "validate", Identity: plugin.Identity{Project: project.Project, Software: name, Destination: destination}, Root: root, Config: settings, Metadata: metadata, Subjects: subjectSelectors(software)}
			var response plugin.ReconcileResponse
			if err := ops.call(ctx, connection.Operation, "validate", request, &response); err != nil {
				return nil, nil, fmt.Errorf("%s/%s: %w", name, destination, err)
			}
			for _, required := range response.Requires {
				target, exists := project.Software[required]
				if !exists {
					return nil, nil, fmt.Errorf("%s/%s references unknown software %q", name, destination, required)
				}
				if _, exists := target.Destinations[destination]; !exists {
					return nil, nil, fmt.Errorf("%s/%s requires %s on the same connection", name, destination, required)
				}
				ref := destinationRef{required, destination}
				if !slices.Contains(dependencies[node], ref) {
					dependencies[node] = append(dependencies[node], ref)
				}
			}
		}
	}
	var ordered []destinationRef
	visited := map[destinationRef]bool{}
	visiting := map[destinationRef]bool{}
	var visit func(destinationRef) error
	visit = func(node destinationRef) error {
		if visiting[node] {
			return fmt.Errorf("software reference cycle at %s/%s", node.Software, node.Destination)
		}
		if visited[node] || !slices.Contains(selected, node.Software) {
			return nil
		}
		visiting[node] = true
		for _, required := range dependencies[node] {
			if err := visit(required); err != nil {
				return err
			}
		}
		delete(visiting, node)
		visited[node] = true
		ordered = append(ordered, node)
		return nil
	}
	for _, node := range nodes {
		if err := visit(node); err != nil {
			return nil, nil, err
		}
	}
	return ordered, dependencies, nil
}

func staticMetadata(value map[string]any) map[string]any {
	result := map[string]any{}
	for key, child := range value {
		switch child := child.(type) {
		case map[string]any:
			if _, fact := child["$fact"]; !fact {
				result[key] = staticMetadata(child)
			}
		case []any:
			// Native list entries may require fields whose facts do not exist yet.
			if !hasFactReference(child) {
				result[key] = child
			}
		default:
			result[key] = child
		}
	}
	return result
}

func mergeOrigins(explicit, derived map[string]string) map[string]string {
	result := make(map[string]string, len(explicit)+len(derived))
	maps.Copy(result, explicit)
	maps.Copy(result, derived)
	return result
}

func (ops *operations) fingerprint(destination config.Destination) string {
	operation, err := ops.operation(destination.Operation)
	if err != nil {
		return config.Fingerprint(destination)
	}
	var schema map[string]any
	_ = json.Unmarshal(operation.ConfigSchema, &schema)
	identity, _ := connectionIdentity(destination.Config, []map[string]any{schema}, schema)
	destination.Config, _ = identity.(map[string]any)
	return config.Fingerprint(destination)
}

// Schema annotations describe credentials for built-in and executable providers alike.
func connectionIdentity(value any, schemas []map[string]any, root map[string]any) (any, bool) {
	var expanded []map[string]any
	for _, schema := range schemas {
		expanded = append(expanded, schemaAnnotations(schema, root, map[string]bool{})...)
	}
	for _, schema := range expanded {
		if schema["writeOnly"] == true {
			return nil, false
		}
	}
	switch value := value.(type) {
	case map[string]any:
		result := map[string]any{}
		for key, child := range value {
			var fields []map[string]any
			for _, schema := range expanded {
				properties, _ := schema["properties"].(map[string]any)
				field, _ := properties[key].(map[string]any)
				if field == nil {
					field, _ = schema["additionalProperties"].(map[string]any)
				}
				fields = append(fields, field)
			}
			if identity, keep := connectionIdentity(child, fields, root); keep {
				result[key] = identity
			}
		}
		return result, true
	case []any:
		result := make([]any, len(value))
		var items []map[string]any
		for _, schema := range expanded {
			item, _ := schema["items"].(map[string]any)
			items = append(items, item)
		}
		for i, child := range value {
			result[i], _ = connectionIdentity(child, items, root)
		}
		return result, true
	default:
		return value, true
	}
}

func schemaAnnotations(schema, root map[string]any, visited map[string]bool) []map[string]any {
	result := []map[string]any{schema}
	if ref, _ := schema["$ref"].(string); strings.HasPrefix(ref, "#/") && !visited[ref] {
		visited[ref] = true
		target := root
		for part := range strings.SplitSeq(strings.TrimPrefix(ref, "#/"), "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			target, _ = target[part].(map[string]any)
		}
		result = append(result, schemaAnnotations(target, root, visited)...)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[key].([]any)
		for _, branch := range branches {
			child, _ := branch.(map[string]any)
			result = append(result, schemaAnnotations(child, root, visited)...)
		}
	}
	return result
}
