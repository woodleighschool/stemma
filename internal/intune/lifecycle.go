package intune

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/woodleighschool/stemma/plugin"
)

type lifecycle struct {
	Retention    *plugin.Retention
	Dependencies []relationshipReference
	Supersedes   []relationshipReference
}

type relationshipReference struct {
	Software string
	Install  bool
}

func lifecycleMetadata(m object) (lifecycle, error) {
	var result lifecycle
	if value, exists := m["retention"]; exists {
		retention, ok := value.(object)
		if !ok || len(retention) != 1 {
			return result, errors.New("retention requires keep")
		}
		if err := fields(retention, "keep"); err != nil {
			return result, err
		}
		var r plugin.Retention
		if err := json.Unmarshal(raw(retention), &r); err != nil || r.Keep < 1 {
			return result, errors.New("retention.keep must be a positive integer")
		}
		result.Retention = &r
	}
	for _, category := range []string{"dependencies", "supersedes"} {
		value, owned := m[category]
		if !owned {
			continue
		}
		if m["@odata.type"] != win32Type {
			return result, errors.New("dependencies and supersedes require a Win32 app")
		}
		list, ok := value.([]any)
		limit := 99
		flag := "auto_install"
		if category == "supersedes" {
			limit, flag = 9, "uninstall_previous"
		}
		if !ok || len(list) > limit {
			return result, fmt.Errorf("%s must be an array of at most %d software references", category, limit)
		}
		refs := make([]relationshipReference, 0, len(list))
		seen := map[string]bool{}
		for _, value := range list {
			item, ok := value.(object)
			if !ok {
				return result, errors.New("relationship must be an object")
			}
			if err := fields(item, "software", flag); err != nil {
				return result, err
			}
			name := text(item["software"])
			install, ok := item[flag].(bool)
			if name == "" || !ok || seen[name] {
				return result, fmt.Errorf("%s requires unique software names and explicit %s booleans", category, flag)
			}
			seen[name] = true
			refs = append(refs, relationshipReference{Software: name, Install: install})
		}
		if category == "dependencies" {
			result.Dependencies = refs
		} else {
			result.Supersedes = refs
		}
	}
	return result, nil
}

func (l lifecycle) requires() []string {
	var names []string
	for _, refs := range [][]relationshipReference{l.Dependencies, l.Supersedes} {
		for _, ref := range refs {
			names = append(names, ref.Software)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// pruneContent keeps the active version and the newest keep-1 other committed
// versions, ordered by numeric ID because mobileAppContent has no timestamp.
// Uncommitted versions are removed. Planning an upload passes no active version.
func (c *client) pruneContent(ctx context.Context, appID, active string, keep int, apply bool) ([]plugin.Change, error) {
	versions, err := c.list(ctx, c.content(appID, "", "", ""))
	if err != nil {
		return nil, err
	}
	numbers := map[string]uint64{}
	var publications, stale []string
	for _, version := range versions {
		id := text(version["id"])
		number, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("intune content version %q is not a version number; refusing retention", id)
		}
		numbers[id] = number
		if id == active {
			continue
		}
		files, err := c.list(ctx, c.content(appID, id, "", ""))
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(files, func(file object) bool { return file["isCommitted"] == true }) {
			publications = append(publications, id)
		} else {
			stale = append(stale, id)
		}
	}
	slices.SortFunc(publications, func(a, b string) int { return cmp.Compare(numbers[b], numbers[a]) })
	stale = append(stale, publications[min(keep-1, len(publications)):]...)
	slices.SortFunc(stale, func(a, b string) int { return cmp.Compare(numbers[a], numbers[b]) })
	changes := make([]plugin.Change, 0, len(stale))
	for _, id := range stale {
		changes = append(changes, plugin.Change{Kind: "retention", Field: "contentVersions", Action: "delete", Before: raw(id)})
		if !apply {
			continue
		}
		// A concurrent administrator may have activated a historical version.
		var current object
		if err := c.request(ctx, abs.GET, c.app(appID), nil, &current); err != nil {
			return changes, err
		}
		if text(current["committedContentVersion"]) != active || current["publishingState"] != "published" {
			return changes, errors.New("intune content changed before retention; refusing deletion")
		}
		if err := c.request(ctx, abs.DELETE, c.contentVersion(appID, id), nil, nil); err != nil {
			return changes, err
		}
	}
	if apply && len(stale) != 0 {
		remaining, err := c.list(ctx, c.content(appID, "", "", ""))
		if err != nil {
			return changes, err
		}
		for _, version := range remaining {
			if slices.Contains(stale, text(version["id"])) {
				return changes, errors.New("intune content deletion readback still contains a pruned version")
			}
		}
	}
	return changes, nil
}
