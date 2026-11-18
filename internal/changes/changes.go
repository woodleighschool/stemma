// Package changes turns outcomes into rows that every report surface renders:
// a change headline and its details as styled spans.
package changes

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

type missing struct{}

func decode(data json.RawMessage) any {
	if len(data) == 0 {
		return missing{}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var result any
	if err := decoder.Decode(&result); err != nil {
		return string(data)
	}
	return result
}

func difference(field string, before, after any) []string {
	if reflect.DeepEqual(before, after) {
		return nil
	}
	left, lok := before.(map[string]any)
	right, rok := after.(map[string]any)
	if lok && rok {
		var lines []string
		keys := maps.Clone(left)
		maps.Copy(keys, right)
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			a, exists := left[key]
			if !exists {
				a = missing{}
			}
			b, exists := right[key]
			if !exists {
				b = missing{}
			}
			lines = append(lines, difference(field+"."+Text(key), a, b)...)
		}
		return lines
	}
	_, aText := multiline(before)
	_, bText := multiline(after)
	if aText || bText {
		lines := func(v any) []any {
			text, ok := v.(string)
			if !ok {
				return []any{v}
			}
			var result []any
			for line := range strings.SplitSeq(text, "\n") {
				result = append(result, line)
			}
			return result
		}
		return append([]string{field + ":"}, lineDiff(lines(before), lines(after))...)
	}
	if _, ok := before.(missing); ok && rok {
		return initial(field+" (added)", right)
	}
	if _, ok := after.(missing); ok && lok {
		return initial(field+" (removed)", left)
	}
	// A null or omitted collection has no members, but retain that distinction
	// in the heading instead of silently presenting an empty collection.
	if items, ok := after.([]any); ok && len(items) > 0 && emptyValue(before) {
		lines := difference(field, []any{}, after)
		if len(lines) > 0 && lines[0] == field+":" {
			lines = lines[1:]
		}
		return append([]string{field + ": " + value(before) + " →"}, lines...)
	}
	if items, ok := before.([]any); ok && len(items) > 0 && emptyValue(after) {
		lines := difference(field, before, []any{})
		if len(lines) > 0 && lines[0] == field+":" {
			lines = lines[1:]
		}
		return append([]string{field + ": → " + value(after)}, lines...)
	}
	if a, ok := before.([]any); ok {
		if b, ok := after.([]any); ok {
			return append([]string{field + ":"}, members(field, a, b)...)
		}
	}
	return []string{field + ": " + transition(before, after)}
}

// members compares two collections as a diff, "- " for what goes and "+ " for
// what comes, one member to a line. Members of the documented receipt,
// install and included-app models are matched by identity, so a changed
// member shows only the fields that changed. Other collections keep order.
func members(field string, before, after []any) []string {
	if key := identityKey(field); key != "" {
		am, aok := indexed(before, key)
		bm, bok := indexed(after, key)
		if aok && bok {
			// Removals come first, then changed members, then additions.
			var removed, changed, added []string
			ids := maps.Clone(am)
			maps.Copy(ids, bm)
			for _, id := range slices.Sorted(maps.Keys(ids)) {
				left, had := am[id]
				right, has := bm[id]
				switch {
				case !had:
					added = append(added, "  + "+member(id, right))
				case !has:
					removed = append(removed, "  - "+member(id, left))
				case !reflect.DeepEqual(left, right):
					old, current := memberPair(id, left, right)
					changed = append(changed, "  - "+old, "  + "+current)
				}
			}
			if lines := slices.Concat(removed, changed, added); len(lines) > 0 {
				return lines
			}
			// Reordering can still be an explicit destination change.
		}
	}
	return lineDiff(before, after)
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
func member(id string, fields map[string]any) string {
	parts := make([]string, 0, len(fields))
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		parts = append(parts, Text(key)+": "+value(fields[key]))
	}
	text := Text(id)
	if text != "" && len(parts) > 0 {
		text += ": "
	}
	return text + strings.Join(parts, ", ")
}

// memberPair renders the two versions of a changed member with only the
// fields that differ, each pair of values reading differently.
func memberPair(id string, before, after map[string]any) (string, string) {
	keys := maps.Clone(before)
	maps.Copy(keys, after)
	var old, current []string
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		a, had := before[key]
		b, has := after[key]
		if had && has && reflect.DeepEqual(a, b) {
			continue
		}
		if !had {
			a = missing{}
		}
		if !has {
			b = missing{}
		}
		x, y := distinct(a, b)
		old = append(old, Text(key)+": "+x)
		current = append(current, Text(key)+": "+y)
	}
	return Text(id) + ": " + strings.Join(old, ", "), Text(id) + ": " + strings.Join(current, ", ")
}

func emptyValue(value any) bool {
	if value == nil {
		return true
	}
	_, missing := value.(missing)
	return missing
}

func initial(field string, v any) []string {
	if text, ok := multiline(v); ok {
		rows := []string{field + ":"}
		for line := range strings.SplitSeq(text, "\n") {
			rows = append(rows, "  + "+Text(line))
		}
		return rows
	}
	var rows []string
	switch v := v.(type) {
	case map[string]any:
		if len(v) == 0 {
			return []string{field + ": {}"}
		}
		for _, key := range slices.Sorted(maps.Keys(v)) {
			rows = append(rows, initial(Text(key), v[key])...)
		}
	case []any:
		if len(v) == 0 {
			return []string{field + ": []"}
		}
		// A new collection is every member added.
		for _, line := range members(field, nil, v) {
			rows = append(rows, strings.TrimPrefix(line, "  "))
		}
	default:
		return []string{field + ": " + value(v)}
	}
	result := []string{field + ":"}
	for _, row := range rows {
		result = append(result, "  "+row)
	}
	return result
}

func indexed(items []any, key string) (map[string]map[string]any, bool) {
	result := make(map[string]map[string]any, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		id, ok := fields[key].(string)
		if !ok || id == "" {
			return nil, false
		}
		if _, exists := result[id]; exists {
			return nil, false
		}
		fields = maps.Clone(fields)
		delete(fields, key)
		result[id] = fields
	}
	return result, true
}

// readable renders one member of a collection without an identity.
func readable(item any) string {
	if fields, object := item.(map[string]any); object {
		if slices.ContainsFunc(slices.Collect(maps.Values(fields)), func(v any) bool { _, ok := multiline(v); return ok }) {
			var lines []string
			for _, key := range slices.Sorted(maps.Keys(fields)) {
				if text, ok := multiline(fields[key]); ok {
					lines = append(lines, Text(key)+":")
					for line := range strings.SplitSeq(text, "\n") {
						lines = append(lines, "  "+Text(line))
					}
				} else {
					lines = append(lines, Text(key)+": "+value(fields[key]))
				}
			}
			return strings.Join(lines, "\n")
		}
		return member("", fields)
	}
	return value(item)
}

// lineDiff compares collections member by member: exact renderings decide
// what changed, and readable ones show it, unless two different members of
// the change would read the same.
func lineDiff(before, after []any) []string {
	shown := map[string]string{}
	exact := func(items []any) string {
		var text strings.Builder
		for _, item := range items {
			data, _ := json.Marshal(item)
			shown[string(data)] = readable(item)
			text.Write(data)
			text.WriteByte('\n')
		}
		return text.String()
	}
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(exact(before), exact(after))
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)
	type change struct{ prefix, member string }
	var changed []change
	seen := map[string]string{}
	ambiguous := map[string]bool{}
	for _, diff := range diffs {
		if diff.Type == diffmatchpatch.DiffEqual {
			continue
		}
		prefix := "  + "
		if diff.Type == diffmatchpatch.DiffDelete {
			prefix = "  - "
		}
		for line := range strings.SplitSeq(strings.TrimSuffix(diff.Text, "\n"), "\n") {
			text := shown[line]
			if other, ok := seen[text]; ok && other != line {
				ambiguous[text] = true
			}
			seen[text] = line
			changed = append(changed, change{prefix, line})
		}
	}
	result := make([]string, 0, len(changed))
	for _, c := range changed {
		text := shown[c.member]
		if ambiguous[text] {
			text = Text(c.member)
		}
		for line := range strings.SplitSeq(text, "\n") {
			result = append(result, c.prefix+line)
		}
	}
	return result
}

func multiline(v any) (string, bool) {
	text, ok := v.(string)
	return text, ok && strings.Contains(text, "\n")
}

// plural counts a noun; an empty noun leaves the count and its space.
func plural(count int, noun string) string {
	if count != 1 && noun != "" {
		noun += "s"
	}
	return strconv.Itoa(count) + " " + noun
}

func transition(before, after any) string {
	a, b := distinct(before, after)
	return a + " → " + b
}

// distinct renders two values so that different ones read differently:
// shortened when that keeps them apart, in full, or exactly as JSON.
func distinct(before, after any) (string, string) {
	a, b := value(before), value(after)
	if a == b && !reflect.DeepEqual(before, after) {
		a, b = fullValue(before), fullValue(after)
		if a == b {
			left, _ := json.Marshal(before)
			right, _ := json.Marshal(after)
			a, b = Text(string(left)), Text(string(right))
		}
	}
	return a, b
}

func value(v any) string {
	if s, ok := v.(string); ok && len(s) == 64 {
		if _, err := hex.DecodeString(s); err == nil {
			return s[:12] + "…"
		}
	}
	return fullValue(v)
}

func fullValue(v any) string {
	if _, ok := v.(missing); ok {
		return "(absent)"
	}
	if s, ok := v.(string); ok {
		if s == "" || s == "null" || s == "(absent)" {
			return strconv.Quote(s)
		}
		return Text(s)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return Text(fmt.Sprint(v))
	}
	return Text(string(data))
}

// Text escapes the characters in one report value that could reshape a report
// or control a terminal, such as newlines, escape sequences and bidirectional
// overrides. Printable text, including non-ASCII letters, is unchanged.
func Text(text string) string {
	var result strings.Builder
	for _, r := range text {
		if strconv.IsPrint(r) {
			result.WriteRune(r)
		} else {
			quoted := strconv.QuoteRune(r)
			result.WriteString(quoted[1 : len(quoted)-1])
		}
	}
	return result.String()
}
