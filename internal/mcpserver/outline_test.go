package mcpserver

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/intune"
)

func TestFieldsKeepDestinationVariants(t *testing.T) {
	contract, err := json.Marshal(intune.MetadataSchema())
	if err != nil {
		t.Fatal(err)
	}
	got, err := fields(contract, "assignments")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range got {
		if strings.HasPrefix(line, "assignments[].notifications ") {
			if !strings.Contains(line, "; only Win32 app") {
				t.Fatalf("notifications lost its platform: %s", line)
			}
			return
		}
	}
	t.Fatalf("notifications missing from %v", got)
}

func TestFieldsKeepDetectionPropertyVariants(t *testing.T) {
	contract, err := json.Marshal(intune.MetadataSchema())
	if err != nil {
		t.Fatal(err)
	}
	got, err := fields(contract, "detection.property")
	if err != nil {
		t.Fatal(err)
	}
	want := "detection[].property (string: exists|version|size_mb|modified|created [type=file] or string: exists|does_not_exist|string|integer|version [type=registry]; only type=file|registry)"
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("detection properties: %v; want %s", got, want)
	}
}

func TestFieldsKeepEveryDestinationType(t *testing.T) {
	contract, err := json.Marshal(intune.MetadataSchema())
	if err != nil {
		t.Fatal(err)
	}
	got, err := fields(contract, "type")
	if err != nil {
		t.Fatal(err)
	}
	want := "type (= win32 [Win32 app] or pkg|dmg [Mac app] or = lob [Mac line-of-business app])"
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("destination types: %v; want %s", got, want)
	}
}

func TestFieldsKeepBooleanConstants(t *testing.T) {
	contract := `{"oneOf":[
		{"properties":{"enabled":{"const":true}},"required":["enabled"]},
		{"properties":{"enabled":{"const":false}},"required":["enabled"]}
	]}`
	got, err := fields([]byte(contract), "enabled")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"enabled* (true|false)"}) {
		t.Fatalf("boolean constants: %v", got)
	}
}

const contract = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Display\n  name."},
    "arch": {"enum": ["x86", "x64", "arm64"]},
    "channel": {"type": "string", "enum": ["stable", "beta"], "default": "stable"},
    "version": {"type": ["string", "null"], "pattern": "^[0-9.]+$"},
    "requires": {"type": "array", "items": {"anyOf": [{"type": "string"}, {"type": "object", "properties": {"name": {"type": "string"}}}]}},
    "source": {
      "type": "object",
      "x-stemma-input": true,
      "oneOf": [
        {"type": "object", "properties": {"resource": {"type": "object", "properties": {"kind": {"type": "string"}}, "required": ["kind"]}}, "required": ["resource"]},
        {"anyOf": [{"required": ["url"]}, {"required": ["resolver"]}]}
      ]
    },
    "detection": {"oneOf": [
      {"type": "object", "properties": {"type": {"const": "msi"}, "product_code": {"type": "string"}}, "required": ["type", "product_code"]},
      {"type": "object", "properties": {"type": {"const": "file"}, "path": {"type": "string"}}, "required": ["type", "path"]},
      {"type": "object", "properties": {"type": {"const": "registry"}, "path": {"type": "string"}, "value": {"type": "string"}}, "required": ["type", "path"]}
    ]},
    "installer": {"anyOf": [
      {"title": "PKG", "type": "object", "properties": {"choices": {"type": "array", "items": {"type": "string"}}}},
      {"title": "DMG", "type": "object", "properties": {"mount": {"type": "boolean", "default": true}}}
    ]},
    "assignments": {"type": "object", "additionalProperties": {"type": "object", "properties": {"intent": {"enum": ["required", "available"]}}, "required": ["intent"]}},
    "installs": {"type": ["array", "null"], "items": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}
  },
  "required": ["name"]
}`

func TestFieldsOutlinesAContract(t *testing.T) {
	got, err := fields([]byte(contract), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"name* (string) Display name.",
		"arch (x86|x64|arm64)",
		"channel (string: stable|beta, default stable)",
		"version (string, matching ^[0-9.]+$ or null)",
		"requires (list of string or object)",
		"requires[].name (string)",
		"source (input; describe resolvers)",
		"source.resource (object)",
		"source.resource.kind* (string)",
		"detection (object)",
		"detection.type* (msi|file|registry)",
		"detection.product_code (string; only type=msi)",
		"detection.path (string; only type=file|registry)",
		"detection.value (string; only type=registry)",
		"installer (object)",
		"installer.choices (list of string; only PKG)",
		"installer.mount (boolean, default true; only DMG)",
		"assignments (map of object)",
		"assignments.<name>.intent* (required|available)",
		"installs (list of object or null)",
		"installs[].path* (string)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("outline:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFieldsNarrowToOneBlock(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{"detection", []string{
			"detection (object)",
			"detection.type* (msi|file|registry)",
			"detection.product_code (string; only type=msi)",
			"detection.path (string; only type=file|registry)",
			"detection.value (string; only type=registry)",
		}},
		{"installs[].path", []string{"installs[].path* (string)"}},
		{"installs.path", []string{"installs[].path* (string)"}},
		{"assignments.students", []string{"assignments.students (object)", "assignments.students.intent* (required|available)"}},
		{"name", []string{"name* (string) Display name."}},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			got, err := fields([]byte(contract), test.path)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(test.want, "\n"))
			}
		})
	}
}

func TestFieldsNameTheFieldsAPathMisses(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"bogus", "no field bogus; fields are name, arch, channel, version, requires, source, detection, installer, assignments, installs"},
		{"detection.bogus", "no field detection.bogus; fields are type, product_code, path, value"},
		{"name.first", "name has no fields"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			if _, err := fields([]byte(contract), test.path); err == nil || err.Error() != test.want {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}
