package changes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestSemanticChanges(t *testing.T) {
	for _, test := range []struct {
		name, field, before, after string
		want                       []string
	}{
		{"scalar", "package.version", `"1.0"`, `"2.0"`, []string{"package.version: 1.0 -> 2.0"}},
		{"missing and null", "value", "", "null", []string{"value: (absent) -> null"}},
		{"null and empty", "value", "null", `""`, []string{`value: null -> ""`}},
		{"changed type", "value", `"1"`, `1`, []string{`value: "1" -> 1`}},
		{"empty collection", "value", `null`, `[]`, []string{`value: null -> []`}},
		{"large integer", "size", "9007199254740992", "9007199254740993", []string{"size: 9007199254740992 -> 9007199254740993"}},
		{"script", "package.postinstall_script", `"#!/bin/sh\necho old\nexit 0\n"`, `"#!/bin/sh\necho new\nexit 0\n"`, []string{"package.postinstall_script: changed (3 lines)"}},
		{"added script", "package.postinstall_script", `null`, `"#!/bin/sh\nexit 0\n"`, []string{"package.postinstall_script: null -> 2 lines"}},
		{"receipt identity", "package.receipts", `[{"packageid":"com.example.pkg","version":"1"}]`, `[{"packageid":"com.example.pkg","version":"2"}]`, []string{"package.receipts[com.example.pkg].version: 1 -> 2"}},
		{"removed object field", "metadata", `{"a":false,"b":1}`, `{"a":false}`, []string{"metadata.b: 1 -> (absent)"}},
		{"unchanged", "field", `{"a":1}`, `{"a":1}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Difference(test.field, json.RawMessage(test.before), json.RawMessage(test.after))
			if strings.Join(got, "\n") != strings.Join(test.want, "\n") {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}

func TestCreatesAndRetentionDescribeObjects(t *testing.T) {
	for _, test := range []struct {
		change plugin.Change
		want   string
	}{
		{plugin.Change{Action: "create", Field: "package", After: json.RawMessage(`{"version":"2","uninstallable":false}`)}, "create package\n  uninstallable: false\n  version: 2"},
		{plugin.Change{Action: "create", Field: "package", After: json.RawMessage(`{"blocking_applications":["Example"],"receipts":[]}`)}, "create package\n  blocking_applications:\n    * Example\n  receipts: []"},
		{plugin.Change{Action: "delete", Kind: "retention", Field: "package", Before: json.RawMessage(`"1.0"`)}, "delete package: 1.0 (retention)"},
	} {
		if got := strings.Join(Lines(test.change), "\n"); got != test.want {
			t.Fatalf("got %q want %q", got, test.want)
		}
	}
}

func TestTextEscapesTerminalControlsAndKeepsPrintableText(t *testing.T) {
	text := "echo \"quoted\"\\path\n\x1b[2J café \u202e"
	want := `echo "quoted"\path\n\x1b[2J café \u202e`
	if got := Text(text); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHashesWithSamePrefixRemainDistinguishable(t *testing.T) {
	before := strings.Repeat("a", 64)
	after := strings.Repeat("a", 63) + "b"
	got := Difference("sha256", json.RawMessage(`"`+before+`"`), json.RawMessage(`"`+after+`"`))
	if len(got) != 1 || !strings.Contains(got[0], before+" -> "+after) {
		t.Fatalf("hidden difference: %v", got)
	}
}

func TestCollectionReorderIsVisible(t *testing.T) {
	got := strings.Join(Difference("assignments", json.RawMessage(`["a","b"]`), json.RawMessage(`["b","a"]`)), "\n")
	if !strings.Contains(got, "  + ") || !strings.Contains(got, "  - ") {
		t.Fatalf("reorder hidden: %s", got)
	}
}

func TestMultilineValuesShowSizeWithoutContents(t *testing.T) {
	script := json.RawMessage(`"#!/bin/sh\ncurl -H \"Authorization: secret\" https://example.test\n"`)
	for _, change := range []plugin.Change{
		{Action: "set", Field: "package.postinstall_script", Before: json.RawMessage(`"exit 0\n"`), After: script},
		{Action: "create", Field: "package", After: json.RawMessage(`{"postinstall_script":` + string(script) + `}`)},
		{Action: "create", Field: "script", After: script},
		{Action: "delete", Field: "script", Before: script},
		{Action: "set", Field: "package", Before: json.RawMessage(`{}`), After: json.RawMessage(`{"postinstall_script":` + string(script) + `}`)},
	} {
		got := strings.Join(Lines(change), "\n")
		if strings.Contains(got, "secret") || !strings.Contains(got, "2 lines") {
			t.Fatalf("%s %s: %s", change.Action, change.Field, got)
		}
	}
}

func TestAddedInstallHasReadableValues(t *testing.T) {
	installs := strings.Join(Difference("package.installs", json.RawMessage(`[]`), json.RawMessage(`[{"path":"/Applications/Example.app","type":"application","CFBundleShortVersionString":"2"}]`)), "\n")
	for _, want := range []string{"package.installs[/Applications/Example.app] (added)", "CFBundleShortVersionString: 2", "type: application"} {
		if !strings.Contains(installs, want) {
			t.Fatalf("missing %q: %s", want, installs)
		}
	}
}

func TestCollectionDiffRetainsTypesAndFullHashIdentity(t *testing.T) {
	for _, pair := range [][2]string{
		{`["1"]`, `[1]`},
		{`["` + strings.Repeat("a", 64) + `"]`, `["` + strings.Repeat("a", 63) + `b"]`},
	} {
		got := strings.Join(Difference("values", json.RawMessage(pair[0]), json.RawMessage(pair[1])), "\n")
		if !strings.Contains(got, "  - ") || !strings.Contains(got, "  + ") {
			t.Fatalf("hidden change: %s", got)
		}
	}
}

func TestReceiptReorderingDoesNotEraseAReportedChange(t *testing.T) {
	got := strings.Join(Difference("package.receipts", json.RawMessage(`[{"packageid":"a","version":"1"},{"packageid":"b","version":"1"}]`), json.RawMessage(`[{"packageid":"b","version":"1"},{"packageid":"a","version":"1"}]`)), "\n")
	if !strings.Contains(got, "  - ") || !strings.Contains(got, "  + ") {
		t.Fatalf("reported change erased: %s", got)
	}
}

func TestOperationWithoutValueDiffRemainsVisible(t *testing.T) {
	if got := strings.Join(Lines(plugin.Change{Kind: "metadata", Action: "reconcile", Field: "catalogs/testing"}), "\n"); got != "reconcile catalogs/testing" {
		t.Fatalf("operation lost: %s", got)
	}
}
