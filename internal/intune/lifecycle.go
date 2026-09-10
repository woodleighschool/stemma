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

type lifecycle struct {
	Retention    *plugin.Retention
	Dependencies []relationshipReference
	Supersedes   []relationshipReference
}

type relationshipReference struct {
	Software string
	Install  bool
}

type contentPublication struct {
	Payload  string `json:"payload"`
	Sequence uint64 `json:"sequence"`
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

func (b *binding) activate() {
	p := b.Pending
	b.PayloadSHA256, b.EnvelopeSHA256, b.ContentVersion = p.PayloadSHA256, p.EnvelopeSHA256, p.VersionID
	p.Stage = "published"
}

func (b *binding) published() {
	p := b.Pending
	sequence := b.Publications.Record(p.PayloadSHA256)
	if b.Versions == nil {
		b.Versions = map[string]contentPublication{}
	}
	b.Versions[p.VersionID] = contentPublication{Payload: p.PayloadSHA256, Sequence: sequence}
	b.Pending = nil
}

func (c *client) pruneContent(ctx context.Context, b *binding, keep int, apply bool) ([]plugin.Change, error) {
	versions, err := c.list(ctx, c.content(b.AppID, "", "", ""))
	if err != nil {
		return nil, err
	}
	retained := b.Publications.Retained(keep)
	if len(retained) == 0 {
		return nil, nil
	}
	latest := map[string]uint64{}
	for _, version := range versions {
		publication := b.Versions[text(version["id"])]
		latest[publication.Payload] = max(latest[publication.Payload], publication.Sequence)
	}
	if !apply && b.ContentVersion == "" {
		latest[b.Publications.Current] = b.Publications.Order[b.Publications.Current]
	}
	var candidates []string
	for _, version := range versions {
		id := text(version["id"])
		publication, owned := b.Versions[id]
		ordered := publication.Sequence != 0 && b.Publications.Order[publication.Payload] != 0
		if !owned || !ordered || (retained[publication.Payload] && publication.Sequence == latest[publication.Payload]) || id == b.ContentVersion || (b.Pending != nil && id == b.Pending.VersionID) {
			continue
		}
		candidates = append(candidates, id)
	}
	slices.Sort(candidates)
	changes := make([]plugin.Change, 0, len(candidates))
	for _, id := range candidates {
		changes = append(changes, plugin.Change{Kind: "retention", Field: "contentVersions", Action: "delete", Before: raw(id)})
		if !apply {
			continue
		}
		// A concurrent administrator may have activated a historical version.
		var current object
		if err := c.request(ctx, abs.GET, c.app(b.AppID), nil, &current); err != nil {
			return changes, err
		}
		if text(current["committedContentVersion"]) != b.ContentVersion || current["publishingState"] != "published" {
			return changes, errors.New("intune content changed before retention; refusing deletion")
		}
		if err := c.request(ctx, abs.DELETE, c.contentVersion(b.AppID, id), nil, nil); err != nil {
			return changes, err
		}
	}
	if apply && len(candidates) != 0 {
		remaining, err := c.list(ctx, c.content(b.AppID, "", "", ""))
		if err != nil {
			return changes, err
		}
		for _, version := range remaining {
			if slices.Contains(candidates, text(version["id"])) {
				return changes, errors.New("intune content deletion readback still contains a pruned version")
			}
		}
		for _, id := range candidates {
			delete(b.Versions, id)
		}
	}
	return changes, nil
}
