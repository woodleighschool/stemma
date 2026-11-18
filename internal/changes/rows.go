package changes

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/source"
)

// Role says what a span of a row means; each surface styles it.
type Role int

const (
	Plain Role = iota
	// Old is a value before a change, or one shown for context.
	Old
	// New is a value after a change.
	New
	// Notable is a change worth a second look.
	Notable
	Failure
	Blocked
	// Label names a field.
	Label
	Added
	Removed
	// Strong leads a line.
	Strong
)

// Span is text with one role.
type Span struct {
	Role Role
	Text string
}

// Line is one line of spans.
type Line []Span

// String is the line without styling.
func (l Line) String() string {
	var text strings.Builder
	for _, span := range l {
		text.WriteString(span.Text)
	}
	return text.String()
}

// Row describes a subject, its short outcome and the details it owns.
// An unnamed row belongs to its enclosing subject.
type Row struct {
	Subject string
	Summary Line
	Details []Line
}

// Failed keeps the explanation beneath the subject's outcome.
func Failed(subject, message string) Row {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(message, "\t", "  ")), "\n")
	row := Row{Subject: subject, Summary: Line{{Failure, "failed"}}}
	for _, line := range lines {
		if line = strings.TrimRight(line, " "); line != "" {
			row.Details = append(row.Details, Line{{Failure, Text(line)}})
		}
	}
	return row
}

// BlockedBy names the prerequisites a resource or destination is waiting on.
func BlockedBy(subject string, blockers engine.Blockers) Row {
	return Row{Subject: subject, Summary: Line{{Blocked, "blocked"}}, Details: []Line{{{Blocked, "requires " + Text(strings.Join(blockers.Names(), ", "))}}}}
}

// Skipped is the row of a resource the command does not apply to.
func Skipped(reason string) Row {
	return Row{Summary: Line{{Old, "skipped"}}, Details: []Line{{{Old, Text(reason)}}}}
}

// Undeclared is the row of a lock document whose resource the catalog no
// longer declares.
func Undeclared() Row {
	return Row{Summary: Line{{Removed, "removed"}, {Plain, " "}, {Old, "(no longer declared)"}}}
}

// replaced is a value replacing another.
func replaced(before, after string) Line {
	return Line{{Old, Text(before) + " →"}, {Plain, " "}, {New, Text(after)}}
}

// InputRows describe one resource's lock changes. Inputs are named only when
// the resource declares more than one.
func InputRows(inputs []lock.InputChange, named bool) []Row {
	rows := make([]Row, 0, len(inputs))
	for _, input := range inputs {
		row := inputRow(input)
		switch {
		case named:
			row.Subject = input.Input
		case input.Kind == lock.Removed:
			// Without a qualifier, a removed input names itself.
			row.Summary = Line{{Removed, Text(input.Input) + " removed"}}
		}
		rows = append(rows, row)
	}
	return rows
}

func inputRow(change lock.InputChange) Row {
	var before, after map[string]any
	if change.Before != nil {
		before = entryFields(*change.Before)
	}
	if change.After != nil {
		after = entryFields(*change.After)
	}
	var row Row
	switch change.Kind {
	case lock.Added:
		row.Summary = Line{{New, "new " + Text(identity(*change.After))}}
	case lock.Removed:
		return Row{Summary: Line{{Removed, "removed"}}}
	case lock.Updated:
		a, b := change.Before, change.After
		switch {
		case b.Version != "" && b.Version != a.Version:
			row.Summary = replaced(identity(*a), b.Version)
		case b.Version == "" && a.Content.Filename != b.Content.Filename:
			row.Summary = replaced(a.Content.Filename, b.Content.Filename)
		default:
			// The source stated the same version, or none, for new bytes.
			role := New
			if b.Version != "" {
				role = Notable
			}
			row.Summary = Line{{Old, Text(identity(*b))}, {Plain, " "}, {role, "file changed"}}
		}
	case lock.Refreshed:
		row.Summary = Line{{Old, Text(identity(*change.After))}}
		if change.Before.Version != change.After.Version {
			row.Summary = replaced(identity(*change.Before), identity(*change.After))
		}
	}
	delete(before, "version")
	delete(after, "version")
	row.Details = fieldLines(before, after)
	return row
}

// identity names an entry's content: its version, or its filename when the
// source states none.
func identity(entry source.Entry) string {
	if entry.Version != "" {
		return entry.Version
	}
	return entry.Content.Filename
}

// fieldLines are the differences between two entries' fields, one per changed
// value. A declaration is a hash of the catalog's input; only that it changed
// says anything.
func fieldLines(before, after map[string]any) []Line {
	var lines []Line
	keys := maps.Clone(before)
	if keys == nil {
		keys = map[string]any{}
	}
	maps.Copy(keys, after)
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		a, aok := before[key]
		b, bok := after[key]
		if !aok {
			a = missing{}
		}
		if !bok {
			b = missing{}
		}
		if key == "declaration" {
			if a != b {
				lines = append(lines, Line{{Label, "declaration changed"}})
			}
			continue
		}
		for _, line := range difference(key, a, b) {
			lines = append(lines, styled(line))
		}
	}
	return lines
}

// styled colours one rendered difference: a field with its transition, or a
// collection member added or removed.
func styled(line string) Line {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	switch {
	case strings.HasPrefix(trimmed, "+ "):
		return Line{{Plain, indent}, {Added, trimmed}}
	case strings.HasPrefix(trimmed, "- "):
		return Line{{Plain, indent}, {Removed, trimmed}}
	}
	field, rest, ok := strings.Cut(line, ": ")
	if !ok {
		return Line{{Plain, line}}
	}
	if before, after, ok := strings.Cut(rest, " → "); ok {
		return Line{{Label, field + ": "}, {Old, before + " →"}, {Plain, " "}, {New, after}}
	}
	return Line{{Label, field + ": "}, {New, rest}}
}

// PluginRow describes a plugin lock entry an update changed.
func PluginRow(change lock.PluginChange) Row {
	switch {
	case change.After == nil:
		return Undeclared()
	case change.Before == nil:
		return Row{Summary: Line{{New, "new " + ShortDigest(change.After.Digest)}}}
	}
	return Row{Summary: replaced(ShortDigest(change.Before.Digest), ShortDigest(change.After.Digest))}
}

// ShortDigest keeps enough of an algorithm-prefixed digest to recognise it.
func ShortDigest(digest string) string {
	if len(digest) > 19 {
		digest = digest[:19]
	}
	return Text(digest)
}

// entryFields is a lock entry as the lock records it, by top-level field.
func entryFields(entry source.Entry) map[string]any {
	data, err := json.Marshal(entry)
	if err != nil {
		return map[string]any{}
	}
	var fields map[string]any
	if decoded, ok := decode(data).(map[string]any); ok {
		fields = decoded
	}
	return fields
}

// words supplies the command-specific terms in a run summary.
var words = map[string]struct{ changed, unchanged, skipped, pruned string }{
	"update":    {changed: "updated"},
	"prepare":   {changed: "prepared", unchanged: "cached"},
	"signature": {changed: "derived", skipped: "nothing to declare"},
	"icon":      {changed: "created", skipped: "without artwork"},
	"plan":      {changed: "to publish", pruned: "to prune"},
	"apply":     {changed: "published", pruned: "pruned"},
}

// Summary is an outcome command's last line: the resources it selected, how
// many ended in each status, and what retention removes.
func Summary(method string, s engine.Summary) Line {
	var parts []Span
	add := func(count int, text string, role Role) {
		if count == 0 {
			return
		}
		if len(parts) > 0 {
			parts = append(parts, Span{Plain, ", "})
		}
		parts = append(parts, Span{role, plural(count, "") + text})
	}
	word := words[method]
	add(s.Changed, cmp.Or(word.changed, string(engine.StatusChanged)), New)
	add(s.Unchanged, cmp.Or(word.unchanged, string(engine.StatusUnchanged)), Old)
	add(s.Failed, string(engine.StatusFailed), Failure)
	add(s.Blocked, string(engine.StatusBlocked), Blocked)
	add(s.Skipped, cmp.Or(word.skipped, string(engine.StatusSkipped)), Old)
	add(s.NotReached, "not reached", Notable)
	line := Line{{Strong, plural(s.Resources, "resource") + ":"}, {Plain, " "}}
	if len(parts) == 0 {
		return Line{{Strong, plural(s.Resources, "resource") + "."}}
	}
	line = append(line, parts...)
	if s.Removed > 0 && word.pruned != "" {
		line = append(line, Span{Plain, "; " + plural(s.Removed, "package") + " " + word.pruned})
	}
	return append(line, Span{Plain, "."})
}
