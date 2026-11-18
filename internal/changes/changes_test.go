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
		{"scalar", "package.version", `"1.0"`, `"2.0"`, []string{"package.version: 1.0 → 2.0"}},
		{"missing and null", "value", "", "null", []string{"value: (absent) → null"}},
		{"null and empty", "value", "null", `""`, []string{`value: null → ""`}},
		{"changed type", "value", `"1"`, `1`, []string{`value: "1" → 1`}},
		{"empty collection", "value", `null`, `[]`, []string{`value: null → []`}},
		{"large integer", "size", "9007199254740992", "9007199254740993", []string{"size: 9007199254740992 → 9007199254740993"}},
		{"script", "package.postinstall_script", `"#!/bin/sh\necho old\nexit 0\n"`, `"#!/bin/sh\necho new\nexit 0\n"`, []string{"package.postinstall_script:", "  - echo old", "  + echo new"}},
		{"added script", "package.postinstall_script", `null`, `"#!/bin/sh\nexit 0\n"`, []string{"package.postinstall_script:", "  - null", "  + #!/bin/sh", "  + exit 0", `  + ""`}},
		{"receipt identity", "package.receipts", `[{"packageid":"com.example.pkg","version":"1"}]`, `[{"packageid":"com.example.pkg","version":"2"}]`, []string{"package.receipts:", "  - com.example.pkg: version: 1", "  + com.example.pkg: version: 2"}},
		{"removed object field", "metadata", `{"a":false,"b":1}`, `{"a":false}`, []string{"metadata.b: 1 → (absent)"}},
		{"unchanged", "field", `{"a":1}`, `{"a":1}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := compare(test.field, json.RawMessage(test.before), json.RawMessage(test.after))
			if strings.Join(got, "\n") != strings.Join(test.want, "\n") {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}

func TestTextEscapesTerminalControlsAndKeepsPrintableText(t *testing.T) {
	text := "echo \"quoted\"\\path\n\x1b[2J café -> next · two  spaces \u202e"
	want := `echo "quoted"\path\n\x1b[2J café -> next · two  spaces \u202e`
	if got := Text(text); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHashesWithSamePrefixRemainDistinguishable(t *testing.T) {
	before := strings.Repeat("a", 64)
	after := strings.Repeat("a", 63) + "b"
	got := compare("sha256", json.RawMessage(`"`+before+`"`), json.RawMessage(`"`+after+`"`))
	if len(got) != 1 || !strings.Contains(got[0], before+" → "+after) {
		t.Fatalf("hidden difference: %v", got)
	}
}

func TestCollectionReorderIsVisible(t *testing.T) {
	got := strings.Join(compare("assignments", json.RawMessage(`["a","b"]`), json.RawMessage(`["b","a"]`)), "\n")
	if !strings.Contains(got, "  + ") || !strings.Contains(got, "  - ") {
		t.Fatalf("reorder hidden: %s", got)
	}
}

func TestAddedInstallHasReadableValues(t *testing.T) {
	installs := strings.Join(compare("package.installs", json.RawMessage(`[]`), json.RawMessage(`[{"path":"/Applications/Example.app","type":"application","CFBundleShortVersionString":"2"}]`)), "\n")
	for _, want := range []string{"package.installs:", "  + /Applications/Example.app: CFBundleShortVersionString: 2, type: application"} {
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
		got := strings.Join(compare("values", json.RawMessage(pair[0]), json.RawMessage(pair[1])), "\n")
		if !strings.Contains(got, "  - ") || !strings.Contains(got, "  + ") {
			t.Fatalf("hidden change: %s", got)
		}
	}
}

func TestReceiptReorderingDoesNotEraseAReportedChange(t *testing.T) {
	got := strings.Join(compare("package.receipts", json.RawMessage(`[{"packageid":"a","version":"1"},{"packageid":"b","version":"1"}]`), json.RawMessage(`[{"packageid":"b","version":"1"},{"packageid":"a","version":"1"}]`)), "\n")
	if !strings.Contains(got, "  - ") || !strings.Contains(got, "  + ") {
		t.Fatalf("reported change erased: %s", got)
	}
}

func compare(field string, before, after json.RawMessage) []string {
	return difference(Text(field), decode(before), decode(after))
}

func TestScriptDiffsRetainValues(t *testing.T) {
	script := json.RawMessage(`"#!/bin/sh\ncurl -H \"Authorization: secret\" https://example.test\n"`)
	for _, field := range []plugin.FieldChange{
		{Field: "pkginfo.postinstall_script", Before: json.RawMessage(`"exit 0\n"`), After: script},
		{Field: "pkginfo.postinstall_script", After: script},
		{Field: "pkginfo", After: json.RawMessage(`{"postinstall_script":` + string(script) + `}`)},
		{Field: "pkginfo.postinstall_script", Before: script},
		{Field: "detection", Before: json.RawMessage(`[{"script":"exit 0\n","type":"script"}]`), After: json.RawMessage(`[{"script":` + string(script) + `,"type":"script"}]`)},
	} {
		got := strings.Join(fieldChange(field), "\n")
		if !strings.Contains(got, "Authorization: secret") || strings.Contains(got, "2 lines") {
			t.Fatalf("%+v: %s", field, got)
		}
	}
}
