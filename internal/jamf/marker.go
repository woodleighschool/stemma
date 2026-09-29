package jamf

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// markerPattern matches an identity marker that has a line of the notes to itself.
var markerPattern = regexp.MustCompile(`(?m)^\[stemma:v1 id=([0-9a-f]{64})\]$`)

func marker(identity string) string { return "[stemma:v1 id=" + identity + "]" }

// identities returns the identity of every marker line in notes.
func identities(notes string) []string {
	var found []string
	for _, match := range markerPattern.FindAllStringSubmatch(notes, -1) {
		found = append(found, match[1])
	}
	return found
}

// unmarked returns the text of notes: no marker lines and no trailing newlines,
// so marking it again reproduces the same notes.
func unmarked(notes string) string {
	lines := slices.DeleteFunc(strings.Split(notes, "\n"), markerPattern.MatchString)
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// withMarker keeps the marker on a final line of its own below text.
func withMarker(text, identity string) string {
	if text == "" {
		return marker(identity)
	}
	return text + "\n" + marker(identity)
}

// noteText is the declared notes text, or the remote text while notes are omitted.
func noteText(current *observed, metadata map[string]json.RawMessage) string {
	if _, declared := metadata["notes"]; declared || current == nil {
		return stringField(metadata, "notes")
	}
	return unmarked(stringField(current.Fields, "notes"))
}

func markedFields(current *observed, declared map[string]json.RawMessage, identity string) map[string]json.RawMessage {
	fields := maps.Clone(declared)
	fields["notes"] = raw(withMarker(noteText(current, declared), identity))
	return fields
}
