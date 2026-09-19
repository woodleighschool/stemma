package engine

import (
	"bytes"
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
	dests := map[destinationRef]destinationPlan{}
	// Validate the declared graph, even when only some publications are selected.
	for _, key := range sortedKeys(plans) {
		software := plans[key]
		for _, destination := range sortedKeys(software.Destinations) {
			node := destinationRef{key, destination}
			plan := destinationPlan{peers: map[string]json.RawMessage{}}
			references, err := publicationReferences(destinationMetadata(software.Destinations[destination]))
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", key, destination, err)
			}
			for _, reference := range references {
				peer, exists := plans[reference.Key()]
				if !exists {
					return nil, fmt.Errorf("%s/%s: unknown resource %s", key, destination, reference.Key())
				}
				metadata, exists := peer.Destinations[destination]
				if !exists {
					return nil, fmt.Errorf("%s/%s: resource %s does not publish to destination %s", key, destination, reference.Key(), destination)
				}
				plan.peers[reference.Key()], err = json.Marshal(staticMetadata(destinationMetadata(metadata)))
				if err != nil {
					return nil, err
				}
				ref := destinationRef{reference.Key(), destination}
				if !slices.Contains(plan.requires, ref) {
					plan.requires = append(plan.requires, ref)
				}
			}
			dests[node] = plan
		}
	}
	visited, visiting := map[destinationRef]bool{}, map[destinationRef]bool{}
	var visit func(destinationRef) error
	visit = func(node destinationRef) error {
		if visiting[node] {
			return fmt.Errorf("publication reference cycle at %s/%s", node.Resource, node.Destination)
		}
		if visited[node] {
			return nil
		}
		visiting[node] = true
		for _, required := range dests[node].requires {
			if err := visit(required); err != nil {
				return err
			}
		}
		delete(visiting, node)
		visited[node] = true
		return nil
	}
	for _, key := range sortedKeys(plans) {
		for _, destination := range sortedKeys(plans[key].Destinations) {
			if err := visit(destinationRef{key, destination}); err != nil {
				return nil, err
			}
		}
	}
	result := map[destinationRef]destinationPlan{}
	for _, key := range selected {
		software := plans[key]
		for _, destination := range sortedKeys(software.Destinations) {
			node := destinationRef{key, destination}
			plan := dests[node]
			plan.requires = slices.DeleteFunc(plan.requires, func(ref destinationRef) bool { return !slices.Contains(selected, ref.Resource) })
			connection := project.Destinations[destination]
			metadata, _ := json.Marshal(staticMetadata(destinationMetadata(software.Destinations[destination])))
			settings, _ := json.Marshal(connection.Config)
			request := plugin.ReconcileRequest{Method: "validate", Identity: plugin.Identity{Project: project.Project, Resource: software.Resource.Reference(), Destination: destination}, Root: root, Config: settings, Metadata: metadata, Subjects: software.Subjects, Peers: plan.peers}
			if err := ops.call(ctx, connection.Operation, "validate", request, nil); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", key, destination, err)
			}
			result[node] = plan
		}
	}
	return result, nil
}

// resource is reserved Stemma syntax within destination metadata, like $fact.
// Only the reference is decoded here; surrounding native properties stay opaque.
func publicationReferences(value any) ([]plugin.ResourceReference, error) {
	var references []plugin.ResourceReference
	switch value := value.(type) {
	case map[string]any:
		for _, key := range sortedKeys(value) {
			if key == "resource" {
				data, err := json.Marshal(value[key])
				if err != nil {
					return nil, err
				}
				var ref plugin.ResourceReference
				decoder := json.NewDecoder(bytes.NewReader(data))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&ref); err != nil {
					return nil, fmt.Errorf("resource: %w", err)
				}
				if err := ref.Validate(); err != nil {
					return nil, err
				}
				references = append(references, ref)
				continue
			}
			nested, err := publicationReferences(value[key])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			references = append(references, nested...)
		}
	case []any:
		for i, item := range value {
			nested, err := publicationReferences(item)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			references = append(references, nested...)
		}
	}
	return references, nil
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
