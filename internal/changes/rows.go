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
	"github.com/woodleighschool/stemma/plugin"
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

// BlockedBy is the row of a resource whose required outputs failed.
func BlockedBy(names []string) Row {
	return Row{Summary: Line{{Blocked, "blocked"}}, Details: []Line{{{Blocked, "blocked by " + Text(strings.Join(names, ", "))}}}}
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

// PublicationRow renders the same change for plan and apply. The command's
// summary distinguishes proposed changes from completed ones.
func PublicationRow(p engine.Publication) Row {
	row := Row{Subject: p.Destination, Summary: PublicationHeadline(p)}
	if p.Error != "" {
		row = Failed(p.Destination, p.Error)
	}
	for _, field := range p.Fields {
		// A filename used as publication identity is already in the headline.
		if p.Error == "" && p.Version != "" && field.Field == "filename" && decode(field.After) == p.Version {
			if len(field.Before) == 0 && p.Previous == "" || decode(field.Before) == p.Previous {
				continue
			}
		}
		for _, line := range fieldChange(field) {
			row.Details = append(row.Details, styled(line))
		}
	}
	for _, u := range p.Uploads {
		upload := Line{{Label, "upload " + Text(u.Name) + ":"}}
		if p.Error != "" || u.Filename != p.Version {
			upload = append(upload, Span{Plain, " " + Text(u.Filename)})
		}
		row.Details = append(row.Details, upload)
		digest := Line{{Label, "  digest: "}}
		if u.Before != "" {
			before, after := distinct(u.Before, u.After)
			digest = append(digest, Span{Old, before + " →"}, Span{Plain, " "}, Span{New, after})
		} else {
			digest = append(digest, Span{New, value(u.After)}, Span{Old, " (previous digest unavailable)"})
		}
		row.Details = append(row.Details, digest)
	}
	for _, repair := range p.Repairs {
		row.Details = append(row.Details, Line{{New, "rebuild " + Text(repair)}})
	}
	if len(p.Removed) > 0 {
		row.Details = append(row.Details, Line{{Removed, "remove " + Text(strings.Join(p.Removed, ", "))}})
	}
	return row
}

// PublicationHeadline identifies the version or content being published.
func PublicationHeadline(p engine.Publication) Line {
	switch {
	case p.Version == "" && (len(p.Uploads) > 0 || len(p.Fields) > 0 || len(p.Removed) > 0 || len(p.Repairs) > 0):
		return Line{{New, "updated"}}
	case p.Version == "":
		return Line{{Old, "unchanged"}}
	case p.Previous == "":
		return Line{{New, "new " + Text(p.Version)}}
	case p.Version != p.Previous:
		return replaced(p.Previous, p.Version)
	case p.Status == engine.StatusUnchanged:
		return Line{{Old, Text(p.Version) + " unchanged"}}
	default:
		return Line{{Old, Text(p.Version)}}
	}
}

// fieldChange renders one field: its initial value, its removal, or the
// difference between its values.
func fieldChange(field plugin.FieldChange) []string {
	name := Text(field.Field)
	before, after := decode(field.Before), decode(field.After)
	if _, absent := before.(missing); absent {
		return initial(name, after)
	}
	lines := difference(name, before, after)
	if len(lines) == 0 {
		return []string{name + ": changed"}
	}
	return lines
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

// words name statuses in each command's summary where they differ from the
// status itself.
var words = map[string]struct{ changed, unchanged, skipped string }{
	"update":    {changed: "updated"},
	"prepare":   {changed: "prepared", unchanged: "cached"},
	"signature": {changed: "derived", skipped: "nothing to declare"},
	"icon":      {changed: "created", skipped: "without artwork"},
	"plan":      {changed: "to publish"},
	"apply":     {changed: "published"},
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
	if s.Removed > 0 && (method == "plan" || method == "apply") {
		verb := " to remove"
		if method == "apply" {
			verb = " removed"
		}
		line = append(line, Span{Plain, "; " + plural(s.Removed, "older version") + verb})
	}
	return append(line, Span{Plain, "."})
}
