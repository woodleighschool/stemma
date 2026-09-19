package intune

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/woodleighschool/stemma/plugin"
)

const (
	dependencyType   = "#microsoft.graph.mobileAppDependency"
	supersedenceType = "#microsoft.graph.mobileAppSupersedence"
)

var errSelfRelationship = errors.New("an Intune app cannot depend on or supersede itself")

func (c *client) desiredRelationships(ctx context.Context, req plugin.ReconcileRequest, tenant *tenantApps, l lifecycle, appID string) ([]object, error) {
	var result []object
	for _, group := range []struct {
		refs  []relationshipReference
		kind  string
		field string
		no    string
		yes   string
	}{
		{l.Dependencies, dependencyType, "dependencyType", "detect", "autoInstall"},
		{l.Supersedes, supersedenceType, "supersedenceType", "update", "replace"},
	} {
		for _, ref := range group.refs {
			if ref.Resource != nil && ref.Resource.Key() == req.Identity.Resource.Key() {
				return nil, errSelfRelationship
			}
			target, err := relationshipApp(ctx, req, tenant, ref)
			if err != nil {
				return nil, err
			}
			if target == appID {
				return nil, errSelfRelationship
			}
			var remote object
			if err := c.request(ctx, abs.GET, c.app(target), nil, &remote); err != nil {
				return nil, fmt.Errorf("read relationship target %q: %w", target, err)
			}
			if remote["@odata.type"] != win32Type || remote["publishingState"] != "published" {
				return nil, fmt.Errorf("relationship target %q must be a published Win32 app", target)
			}
			value := group.no
			if ref.Install {
				value = group.yes
			}
			result = append(result, object{"@odata.type": group.kind, "targetId": target, "targetType": "child", group.field: value})
		}
	}
	return result, nil
}

// relationshipApp uses explicit remote IDs or the publication of an exact resource.
func relationshipApp(ctx context.Context, req plugin.ReconcileRequest, tenant *tenantApps, ref relationshipReference) (string, error) {
	if ref.Resource == nil {
		return ref.AppID, nil
	}
	key := ref.Resource.Key()
	data, exists := req.Peers[key]
	if !exists {
		return "", fmt.Errorf("intune relationship resource %s does not publish to this destination", key)
	}
	var declared struct {
		AppID string `json:"app_id"`
	}
	if err := json.Unmarshal(data, &declared); err != nil {
		return "", fmt.Errorf("intune relationship resource %s metadata: %w", key, err)
	}
	if declared.AppID != "" {
		return declared.AppID, nil
	}
	identity := plugin.Identity{Project: req.Identity.Project, Resource: *ref.Resource, Destination: req.Identity.Destination}
	id, err := tenant.find(ctx, markerIdentity(identity))
	if err != nil {
		return "", fmt.Errorf("intune relationship resource %s: %w", key, err)
	}
	if id == "" {
		return "", fmt.Errorf("intune relationship resource %s is not published to this destination yet", key)
	}
	return id, nil
}

func relationshipCategory(item object) string {
	switch item["@odata.type"] {
	case dependencyType:
		return "dependencies"
	case supersedenceType:
		return "supersedes"
	default:
		return ""
	}
}

func outgoingRelationships(items []object) ([]object, error) {
	result := make([]object, 0, len(items))
	for _, item := range items {
		switch item["targetType"] {
		case "parent":
			continue
		case "child":
			if text(item["targetId"]) == "" {
				return nil, errors.New("intune relationship omitted targetId")
			}
		default:
			return nil, errors.New("intune relationship direction is unknown; refusing to replace relationships")
		}
		result = append(result, item)
	}
	return result, nil
}

func mergeRelationships(existing, desired []object, l lifecycle) ([]object, bool, error) {
	current, err := outgoingRelationships(existing)
	if err != nil {
		return nil, false, err
	}
	result := make([]object, 0, len(current)+len(desired))
	for _, item := range current {
		category := relationshipCategory(item)
		if category == "dependencies" && l.Dependencies != nil || category == "supersedes" && l.Supersedes != nil {
			continue
		}
		result = append(result, item)
	}
	result = append(result, desired...)
	return result, !sameRelationships(current, result), nil
}

func sameRelationships(a, b []object) bool {
	keys := func(items []object) []string {
		var keys []string
		for _, item := range items {
			keys = append(keys, string(raw([]any{item["@odata.type"], item["targetId"], item["targetType"], item["dependencyType"], item["supersedenceType"]})))
		}
		slices.Sort(keys)
		return keys
	}
	return slices.Equal(keys(a), keys(b))
}

type relationshipEdge struct {
	parent, child, category string
}

// Inspect the connected subgraph because inbound references also consume native
// relationship limits. Bound traversal rather than listing unrelated tenant apps.
func (c *client) checkRelationships(ctx context.Context, appID string, desired []object, current []object) error {
	root := appID
	if root == "" {
		root = "new-app"
	}
	var edges []relationshipEdge
	queue := []string{root}
	seen := map[string]bool{}
	for len(queue) != 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(seen) > 200 {
			return errors.New("intune relationship graph exceeds the inspection limit")
		}
		var relationships []object
		if id == root {
			relationships = append(relationships, desired...)
			for _, item := range current {
				if item["targetType"] == "parent" {
					relationships = append(relationships, item)
				}
			}
		} else {
			var err error
			relationships, err = c.list(ctx, c.relationships(id))
			if err != nil {
				return err
			}
		}
		for _, item := range relationships {
			category := relationshipCategory(item)
			if category == "" {
				return errors.New("unsupported relationship type in Intune graph")
			}
			target := text(item["targetId"])
			if target == "" || !enum(item["targetType"], "child", "parent") {
				return errors.New("incomplete Intune relationship in graph")
			}
			parent, child := id, target
			if item["targetType"] == "parent" {
				parent, child = target, id
			}
			// Other nodes still report outgoing root edges removed by this plan.
			if id != root && parent == root {
				continue
			}
			edges = append(edges, relationshipEdge{parent, child, category})
			queue = append(queue, target)
		}
	}
	return validateRelationshipGraph(root, edges)
}

func validateRelationshipGraph(root string, edges []relationshipEdge) error {
	children := map[string][]string{}
	pairs := map[[2]string]string{}
	superseded := map[string]bool{}
	for _, edge := range edges {
		if edge.category == "supersedes" {
			superseded[edge.child] = true
		}
	}
	for _, edge := range edges {
		if edge.category == "dependencies" && superseded[edge.child] {
			return errors.New("intune dependencies cannot target an app that is superseded in the same graph")
		}
		key := [2]string{edge.parent, edge.child}
		if category, exists := pairs[key]; exists && category != edge.category {
			return errors.New("the same app cannot be both a dependency and superseded app")
		}
		pairs[key] = edge.category
		children[edge.parent] = append(children[edge.parent], edge.child)
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return errors.New("intune relationships contain a cycle")
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, child := range children[id] {
			if err := visit(child); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range children {
		if err := visit(id); err != nil {
			return err
		}
	}
	for category, limit := range map[string]int{"dependencies": 100, "supersedes": 10} {
		connected := map[string]bool{root: true}
		for {
			before := len(connected)
			for _, edge := range edges {
				if edge.category == category && (connected[edge.parent] || connected[edge.child]) {
					connected[edge.parent], connected[edge.child] = true, true
				}
			}
			if len(connected) > limit {
				return fmt.Errorf("intune %s graph exceeds %d apps", category, limit)
			}
			if len(connected) == before {
				break
			}
		}
	}
	return nil
}
