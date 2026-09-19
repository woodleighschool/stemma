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

type destinationRef struct {
	Resource    string
	Destination string
}

type destinationPlan struct {
	requires []destinationRef
	peers    map[string]json.RawMessage
}

// Unselected peers contribute metadata without becoming part of the run.
func planDestinations(ctx context.Context, project config.Project, plans map[string]resourcePlan, ops *operations, root string, selected []string) (map[destinationRef]destinationPlan, error) {
	destinations := map[destinationRef]destinationPlan{}
	declared := sortedKeys(plans)
	var nodes []destinationRef
	for _, name := range selected {
		software := plans[name]
		for _, destination := range sortedKeys(software.Destinations) {
			node := destinationRef{name, destination}
			plan := destinationPlan{peers: map[string]json.RawMessage{}}
			nodes = append(nodes, node)
			connection := project.Destinations[destination]
			metadata, _ := json.Marshal(staticMetadata(destinationMetadata(software.Destinations[destination])))
			settings, _ := json.Marshal(connection.Config)
			request := plugin.ReconcileRequest{Method: "validate", Identity: plugin.Identity{Project: project.Project, Software: software.Resource.Metadata.Name, Destination: destination}, Root: root, Config: settings, Metadata: metadata, Subjects: software.Subjects}
			var response plugin.ReconcileResponse
			if err := ops.call(ctx, connection.Operation, "validate", request, &response); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", name, destination, err)
			}
			for _, required := range response.Requires {
				if ref, found := peer(plans, declared, destination, required); found {
					metadata, _ := json.Marshal(staticMetadata(destinationMetadata(plans[ref.Resource].Destinations[destination])))
					plan.peers[required] = metadata
				}
				ref, found := peer(plans, selected, destination, required)
				if !found {
					plugin.Logger(ctx).DebugContext(ctx, "Required resource is not reconciled in this run", "resource", name, "destination", destination, "requires", required)
					continue
				}
				if ref != node && !slices.Contains(plan.requires, ref) {
					plan.requires = append(plan.requires, ref)
				}
			}
			destinations[node] = plan
		}
	}
	visited := map[destinationRef]bool{}
	visiting := map[destinationRef]bool{}
	var visit func(destinationRef) error
	visit = func(node destinationRef) error {
		if visiting[node] {
			return fmt.Errorf("software reference cycle at %s/%s", node.Resource, node.Destination)
		}
		if visited[node] {
			return nil
		}
		visiting[node] = true
		for _, required := range destinations[node].requires {
			if err := visit(required); err != nil {
				return err
			}
		}
		delete(visiting, node)
		visited[node] = true
		return nil
	}
	for _, node := range nodes {
		if err := visit(node); err != nil {
			return nil, err
		}
	}
	return destinations, nil
}

// peer finds, among keys, the resource with a name that publishes to a destination.
func peer(plans map[string]resourcePlan, keys []string, destination, name string) (destinationRef, bool) {
	for _, key := range keys {
		if plans[key].Resource.Metadata.Name != name {
			continue
		}
		if _, ok := plans[key].Destinations[destination]; ok {
			return destinationRef{key, destination}, true
		}
	}
	return destinationRef{}, false
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
	result := make(map[string]string)
	maps.Copy(result, explicit)
	maps.Copy(result, derived)
	return result
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
