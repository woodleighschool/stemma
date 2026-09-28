package jamf

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// policyScope names a policy's targets, limitations and exclusions exactly as
// in Jamf.
type policyScope struct {
	AllComputers   *bool              `json:"all_computers,omitempty"`
	Computers      *[]string          `json:"computers,omitempty"`
	ComputerGroups *[]string          `json:"computer_groups,omitempty"`
	Buildings      *[]string          `json:"buildings,omitempty"`
	Departments    *[]string          `json:"departments,omitempty"`
	Limitations    *policyLimitations `json:"limitations,omitempty"`
	Exclusions     *policyExclusions  `json:"exclusions,omitempty"`
}
type policyLimitations struct {
	NetworkSegments *[]string `json:"network_segments,omitempty"`
}
type policyExclusions struct {
	Computers       *[]string `json:"computers,omitempty"`
	ComputerGroups  *[]string `json:"computer_groups,omitempty"`
	Buildings       *[]string `json:"buildings,omitempty"`
	Departments     *[]string `json:"departments,omitempty"`
	NetworkSegments *[]string `json:"network_segments,omitempty"`
}

// A scopeList is one scope collection and the kind of object its names select.
type scopeList struct {
	kind  string
	names *[]string
}

// lists returns the declared scope collections by their Classic API path.
func (s *policyScope) lists() map[string]scopeList {
	lists := map[string]scopeList{}
	if s == nil {
		return lists
	}
	lists["computers"] = scopeList{"computer", s.Computers}
	lists["computer_groups"] = scopeList{"computer group", s.ComputerGroups}
	lists["buildings"] = scopeList{"building", s.Buildings}
	lists["departments"] = scopeList{"department", s.Departments}
	if l := s.Limitations; l != nil {
		lists["limitations/network_segments"] = scopeList{"network segment", l.NetworkSegments}
	}
	if e := s.Exclusions; e != nil {
		lists["exclusions/computers"] = scopeList{"computer", e.Computers}
		lists["exclusions/computer_groups"] = scopeList{"computer group", e.ComputerGroups}
		lists["exclusions/buildings"] = scopeList{"building", e.Buildings}
		lists["exclusions/departments"] = scopeList{"department", e.Departments}
		lists["exclusions/network_segments"] = scopeList{"network segment", e.NetworkSegments}
	}
	return lists
}

func (s *policyScope) validate(owner string) error {
	for path, list := range s.lists() {
		if list.names != nil && slices.ContainsFunc(*list.names, func(name string) bool { return strings.TrimSpace(name) == "" }) {
			return fmt.Errorf("jamf %s scope %s names cannot be empty", owner, path)
		}
	}
	return nil
}

// resolveScope finds the ID of every declared scope object, by list path.
func (c *client) resolveScope(ctx context.Context, s *policyScope) (map[string][]string, error) {
	resolved := map[string][]string{}
	for path, list := range s.lists() {
		if list.names == nil {
			continue
		}
		ids := []string{}
		for _, name := range declaredNames(*list.names) {
			id, err := c.objectID(ctx, list.kind, name)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		resolved[path] = ids
	}
	return resolved, nil
}

// scopeSettings are the declared scope values of a document rooted at root,
// with their objects as resolved IDs.
func scopeSettings(root string, s *policyScope, resolved map[string][]string) []setting {
	if s == nil {
		return nil
	}
	var settings []setting
	if s.AllComputers != nil {
		settings = append(settings, setting{field: "scope.all_computers", value: *s.AllComputers, fragment: fragment(root, *s.AllComputers, "scope", "all_computers"), current: flag("scope", "all_computers")})
	}
	lists := s.lists()
	for _, path := range slices.Sorted(maps.Keys(lists)) {
		list := lists[path]
		if list.names == nil {
			continue
		}
		objects := make([]map[string]string, 0, len(resolved[path]))
		for _, id := range resolved[path] {
			objects = append(objects, map[string]string{"id": id})
		}
		steps := append([]string{"scope"}, strings.Split(path, "/")...)
		settings = append(settings, setting{
			field:    strings.Join(steps, "."),
			value:    declaredNames(*list.names),
			fragment: fragment(root, objects, steps...),
			current:  names(steps...),
		})
	}
	return settings
}

// declaredNames returns names sorted, each once.
func declaredNames(names []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(names)))
}
