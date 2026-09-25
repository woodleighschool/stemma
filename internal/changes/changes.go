// Package changes renders semantic differences for command reports and review descriptions.
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
	"github.com/woodleighschool/stemma/plugin"
)

// Lines describes a destination change without terminal or Markdown formatting.
func Lines(change plugin.Change) []string {
	field := Text(change.Field)
	before, after := decode(change.Before), decode(change.After)
	switch change.Action {
	case "create":
		if _, absent := after.(missing); absent {
			return []string{"create " + field}
		}
		if fields, ok := after.(map[string]any); ok {
			lines := []string{"create " + field}
			for _, key := range slices.Sorted(maps.Keys(fields)) {
				for _, line := range initial(Text(key), fields[key]) {
					lines = append(lines, "  "+line)
				}
			}
			return lines
		}
		return []string{"create " + field + ": " + summary(after)}
	case "delete":
		line := "delete " + field
		if _, absent := before.(missing); !absent {
			line += ": " + summary(before)
		}
		if change.Kind == "retention" {
			line += " (retention)"
		}
		return []string{line}
	case "upload":
		return []string{"upload " + field + ": " + transition(before, after)}
	default:
		lines := difference(field, before, after)
		if len(lines) == 0 {
			return []string{Text(change.Action) + " " + field}
		}
		return lines
	}
}

// Difference compares arbitrary JSON values without interpreting their vocabulary.
func Difference(field string, before, after json.RawMessage) []string {
	return difference(Text(field), decode(before), decode(after))
}

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
	a, aText := multiline(before)
	b, bText := multiline(after)
	if aText && bText {
		return []string{field + ": changed (" + lineRange(a, b) + ")"}
	}
	if aText || bText {
		return []string{field + ": " + summary(before) + " -> " + summary(after)}
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
		return append([]string{field + ": " + value(before) + " ->"}, lines...)
	}
	if items, ok := before.([]any); ok && len(items) > 0 && emptyValue(after) {
		lines := difference(field, before, []any{})
		if len(lines) > 0 && lines[0] == field+":" {
			lines = lines[1:]
		}
		return append([]string{field + ": -> " + value(after)}, lines...)
	}
	if a, ok := before.([]any); ok {
		if b, ok := after.([]any); ok {
			// Identity comes from the documented receipt/install models, not a
			// guessed key in arbitrary plugin data. Other arrays retain order.
			key := ""
			if field == "receipts" || strings.HasSuffix(field, ".receipts") {
				key = "packageid"
			}
			if field == "installs" || strings.HasSuffix(field, ".installs") {
				key = "path"
			}
			if key != "" {
				am, aok := indexed(a, key)
				bm, bok := indexed(b, key)
				if aok && bok {
					var result []string
					keys := maps.Clone(am)
					maps.Copy(keys, bm)
					for _, id := range slices.Sorted(maps.Keys(keys)) {
						left, exists := am[id]
						if !exists {
							left = missing{}
						}
						right, exists := bm[id]
						if !exists {
							right = missing{}
						}
						result = append(result, difference(field+"["+Text(id)+"]", left, right)...)
					}
					if len(result) > 0 {
						return result
					}
					// Reordering can still be an explicit destination change.
					// Do not erase it merely because the keyed members match.
				}
			}
			return append([]string{field + ":"}, lineDiff(arrayLines(a), arrayLines(b))...)
		}
	}
	return []string{field + ": " + transition(before, after)}
}

func emptyValue(value any) bool {
	if value == nil {
		return true
	}
	_, missing := value.(missing)
	return missing
}

func initial(field string, v any) []string {
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
		// Reports mark the collection members a change adds or removes with
		// "+ " and "- ", so members of a whole new or removed value use
		// another bullet.
		for _, item := range v {
			rows = append(rows, "* "+value(item))
		}
	default:
		return []string{field + ": " + summary(v)}
	}
	result := []string{field + ":"}
	for _, row := range rows {
		result = append(result, "  "+row)
	}
	return result
}

func indexed(items []any, key string) (map[string]any, bool) {
	result := make(map[string]any, len(items))
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

func arrayLines(items []any) string {
	var text strings.Builder
	for _, item := range items {
		data, _ := json.Marshal(item)
		text.Write(data)
		text.WriteByte('\n')
	}
	return text.String()
}

// lineDiff compares collections rendered one member per line.
func lineDiff(before, after string) []string {
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(before, after)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)
	var result []string
	for _, diff := range diffs {
		if diff.Type == diffmatchpatch.DiffEqual {
			continue
		}
		prefix := "  + "
		if diff.Type == diffmatchpatch.DiffDelete {
			prefix = "  - "
		}
		for line := range strings.SplitSeq(strings.TrimSuffix(diff.Text, "\n"), "\n") {
			result = append(result, prefix+Text(line))
		}
	}
	return result
}

// multiline reports whether v is text spanning lines. Reports describe such
// values by size: they are usually scripts, which can embed environment values.
func multiline(v any) (string, bool) {
	text, ok := v.(string)
	return text, ok && strings.Contains(text, "\n")
}

func lineCount(text string) int {
	return strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
}

func lineRange(before, after string) string {
	a, b := lineCount(before), lineCount(after)
	if a == b {
		return plural(b, "line")
	}
	return strconv.Itoa(a) + " -> " + plural(b, "line")
}

func plural(count int, noun string) string {
	if count != 1 {
		noun += "s"
	}
	return strconv.Itoa(count) + " " + noun
}

// summary renders a value, describing multi-line text by its length.
func summary(v any) string {
	if text, ok := multiline(v); ok {
		return plural(lineCount(text), "line")
	}
	return value(v)
}

func transition(before, after any) string {
	a, b := value(before), value(after)
	if a == b && !reflect.DeepEqual(before, after) {
		a, b = fullValue(before), fullValue(after)
		if a == b {
			left, _ := json.Marshal(before)
			right, _ := json.Marshal(after)
			a, b = Text(string(left)), Text(string(right))
		}
	}
	return a + " -> " + b
}

func value(v any) string {
	if s, ok := v.(string); ok && len(s) == 64 {
		if _, err := hex.DecodeString(s); err == nil {
			return s[:12] + "..."
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
