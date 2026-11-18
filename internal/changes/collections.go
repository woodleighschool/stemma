package changes

import (
	"maps"
	"reflect"
	"slices"
	"strings"
)

// members matches known collections by the identity their destination uses.
// Ambiguous identities and reordered members retain the ordered list diff.
func members(field string, before, after []any) []string {
	key := identityKey(field)
	if key == "" && field != "detection" {
		return lineDiff(before, after)
	}
	left, beforeIDs := indexed(before, key)
	right, afterIDs := indexed(after, key)
	if left == nil || right == nil {
		return lineDiff(before, after)
	}
	// Only the relative order of members present on both sides can change.
	commonBefore := slices.DeleteFunc(beforeIDs, func(id string) bool { return right[id] == nil })
	commonAfter := slices.DeleteFunc(afterIDs, func(id string) bool { return left[id] == nil })
	if !slices.Equal(commonBefore, commonAfter) {
		return lineDiff(before, after)
	}
	var removed, changed, added []string
	for _, id := range slices.Sorted(maps.Keys(left)) {
		old := left[id]
		current, ok := right[id]
		if !ok {
			removed = append(removed, memberLines("  - ", field, id, key, old)...)
		} else if !reflect.DeepEqual(old, current) {
			changed = append(changed, "  "+Text(id)+":")
			label := func(key string) string {
				if field == "detection" && key == "value" && old["property"] == "version" && current["property"] == "version" {
					return "version"
				}
				return memberLabel(field, key)
			}
			for _, line := range objectDiff(old, current, label) {
				changed = append(changed, "    "+line)
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(right)) {
		if left[id] == nil {
			added = append(added, memberLines("  + ", field, id, key, right[id])...)
		}
	}
	return slices.Concat(removed, changed, added)
}

func memberLines(prefix, field, id, key string, fields map[string]any) []string {
	if key != "" {
		fields = maps.Clone(fields)
		delete(fields, key)
		return []string{prefix + member(field, id, fields)}
	}
	var lines []string
	for line := range strings.SplitSeq(readable(fields), "\n") {
		lines = append(lines, prefix+line)
	}
	return lines
}

func indexed(items []any, key string) (map[string]map[string]any, []string) {
	result := make(map[string]map[string]any, len(items))
	var order []string
	for _, item := range items {
		fields, _ := item.(map[string]any)
		id, _ := fields[key].(string)
		if key == "" {
			id = detectionSubject(fields)
		}
		if id == "" || result[id] != nil {
			return nil, nil
		}
		result[id] = fields
		order = append(order, id)
	}
	return result, order
}

// File and registry rules address a named location. MSI and script rules are
// unique by type within the detection collection.
func detectionSubject(rule map[string]any) string {
	kind, _ := rule["type"].(string)
	switch kind {
	case "file":
		path, _ := rule["path"].(string)
		name, _ := rule["name"].(string)
		if path != "" && name != "" {
			return "file " + strings.TrimRight(path, `\`) + `\` + name
		}
	case "registry":
		key, _ := rule["key"].(string)
		value, _ := rule["value_name"].(string)
		if key != "" {
			return strings.TrimRight("registry "+key+`\`+value, `\`)
		}
	case "msi", "script":
		return kind
	}
	return ""
}

// identityKey names the field that identifies a member of a documented
// collection, not a guessed key in arbitrary plugin data.
func identityKey(field string) string {
	switch {
	case field == "receipts" || strings.HasSuffix(field, ".receipts"):
		return "packageid"
	case field == "installs" || strings.HasSuffix(field, ".installs"):
		return "path"
	case field == "included_apps":
		return "id"
	}
	return ""
}

// member renders one collection member on a line: its identity, then its
// fields in name order.
func member(field, id string, fields map[string]any) string {
	parts := make([]string, 0, len(fields))
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		parts = append(parts, memberLabel(field, key)+": "+value(fields[key]))
	}
	text := Text(id)
	if text != "" && len(parts) > 0 {
		text += ": "
	}
	return text + strings.Join(parts, ", ")
}

func memberLabel(collection, key string) string {
	if collection == "pkginfo.receipts" && key == "installed_size" {
		return "installed_size (KiB)"
	}
	return Text(key)
}
