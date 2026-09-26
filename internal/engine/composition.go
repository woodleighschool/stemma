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
	"github.com/woodleighschool/stemma/internal/expression"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/plugin"
)

type destinationRef struct {
	Resource    string
	Destination string
}

type destinationPlan struct {
	requires []destinationRef
	peers    map[string]json.RawMessage
	// environment reports whether the metadata, or a peer's, reads env values,
	// which only publication needs.
	environment bool
}

// Unselected peers contribute metadata without becoming part of the run.
// Environment evaluates metadata that reads env values now; otherwise its
// provider validation waits for publication, as metadata reading facts does.
func planDestinations(ctx context.Context, project config.Project, plans map[string]resourcePlan, ops *operations, root string, selected []string, environment bool) (map[destinationRef]destinationPlan, error) {
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
				plan.peers[reference.Key()], err = json.Marshal(destinationMetadata(metadata))
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
			native := destinationMetadata(software.Destinations[destination])
			metadata, err := json.Marshal(native)
			if err != nil {
				return nil, err
			}
			op, err := ops.operation(connection.Operation)
			if err != nil {
				return nil, err
			}
			authoredSchema, err := config.ExpressionSchema(op.MetadataSchema)
			if err != nil {
				return nil, err
			}
			if len(authoredSchema) > 0 {
				if err := plugin.ValidateSchema(authoredSchema, metadata); err != nil {
					return nil, fmt.Errorf("%s/%s metadata: %w", key, destination, err)
				}
			}
			roots, err := expression.Roots(native)
			if err != nil {
				return nil, fmt.Errorf("%s/%s metadata: %w", key, destination, err)
			}
			deferred := slices.Contains(roots, "facts") || slices.Contains(roots, "evidence")
			plan.environment = slices.Contains(roots, "env")
			for _, peer := range sortedKeys(plan.peers) {
				if len(authoredSchema) > 0 {
					if err := plugin.ValidateSchema(authoredSchema, plan.peers[peer]); err != nil {
						return nil, fmt.Errorf("%s/%s peer %s metadata: %w", key, destination, peer, err)
					}
				}
				roots, err := expression.Roots(destinationMetadata(plans[peer].Destinations[destination]))
				if err != nil {
					return nil, fmt.Errorf("%s/%s peer %s metadata: %w", key, destination, peer, err)
				}
				if slices.Contains(roots, "facts") || slices.Contains(roots, "evidence") {
					if !slices.Contains(selected, peer) {
						return nil, fmt.Errorf("%s/%s: peer %s metadata requires preparation", key, destination, peer)
					}
					deferred = true
				}
				plan.environment = plan.environment || slices.Contains(roots, "env")
			}
			if deferred || plan.environment && !environment {
				result[node] = plan
				continue
			}
			// Provider validation only receives evaluated metadata. Preparation
			// supplies contexts for declarations that depend on artifact evidence.
			resolved, _, err := resolveMetadata(software.ResourceResult, native, plugin.Facts{}, nil)
			if err != nil {
				return nil, fmt.Errorf("%s/%s metadata: %w", key, destination, err)
			}
			metadata, err = json.Marshal(resolved)
			if err != nil {
				return nil, err
			}
			peers, err := resolvePeerMetadata(ctx, plans, destination, plan.peers, nil, op.MetadataSchema)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", key, destination, err)
			}
			minimum, err := minimumOS(plugin.Artifact{}, software.MinimumOS)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", key, destination, err)
			}
			request := plugin.ReconcileRequest[json.RawMessage]{Method: "validate", Identity: plugin.Identity{Project: project.Project, Resource: software.Resource.Reference(), Destination: destination}, Root: root, Metadata: metadata, MinimumOS: minimum, Peers: peers}
			if err := ops.call(ctx, connection.Operation, "validate", request, nil); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", key, destination, err)
			}
			result[node] = plan
		}
	}
	return result, nil
}

func resolvePeerMetadata(ctx context.Context, plans map[string]resourcePlan, destination string, peers map[string]json.RawMessage, prepared map[string]preparedResource, schema json.RawMessage) (map[string]json.RawMessage, error) {
	result := make(map[string]json.RawMessage, len(peers))
	for _, key := range sortedKeys(peers) {
		plan := plans[key]
		native := destinationMetadata(plan.Destinations[destination])
		roots, err := expression.Roots(native)
		if err != nil {
			return nil, fmt.Errorf("peer %s metadata: %w", key, err)
		}
		resource, available := prepared[key]
		if (!available || !resource.ready) && (slices.Contains(roots, "facts") || slices.Contains(roots, "evidence")) {
			return nil, fmt.Errorf("peer %s metadata requires preparation", key)
		}
		output := "installer"
		reference, explicitOutput := plan.Destinations[destination]["installer"].(string)
		if explicitOutput {
			output = reference
		}
		artifact, present := resource.outputs[output]
		if available && resource.ready && !present && explicitOutput {
			return nil, fmt.Errorf("peer %s references missing output %s", key, output)
		}
		if present && !artifact.SuppliedFacts && slices.Contains(roots, "facts") {
			artifact.Facts, err = inspection.Read(ctx, artifact.Path)
			if err != nil {
				return nil, fmt.Errorf("peer %s required inspection: %w", key, err)
			}
		}
		metadata, _, err := resolveMetadata(plan.ResourceResult, native, artifact.Facts, artifact.Evidence)
		if err != nil {
			return nil, fmt.Errorf("peer %s metadata: %w", key, err)
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		if len(schema) > 0 {
			if err := plugin.ValidateSchema(schema, data); err != nil {
				return nil, fmt.Errorf("peer %s metadata: %w", key, err)
			}
		}
		result[key] = data
	}
	return result, nil
}

// resource is reserved Stemma syntax within destination metadata.
// Only the reference is decoded here; surrounding native properties stay opaque.
func publicationReferences(value any) ([]plugin.ResourceReference, error) {
	var references []plugin.ResourceReference
	switch value := value.(type) {
	case map[string]any:
		for _, key := range sortedKeys(value) {
			if key == "resource" {
				if expression.Has(value[key]) {
					return nil, fmt.Errorf("resource references must be literal")
				}
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

func mergeOrigins(explicit, derived map[string]string) map[string]string {
	result := make(map[string]string)
	maps.Copy(result, derived)
	maps.Copy(result, explicit)
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
