package munki

import (
	"encoding/json"
	"github.com/woodleighschool/stemma/plugin"
	"testing"
)

func TestSelectedMacApplicationEvidence(t *testing.T) {
	selected := plugin.Subject{Kind: "app", Path: "Tools/Editor.app", InstalledPath: "/Applications/Editor.app", App: &plugin.AppFacts{BundleID: "example.editor", Version: "2.3", Build: "203", MinimumOS: "13.0"}}
	evidence, _ := json.Marshal(selected)
	request := plugin.ReconcileRequest{Prepared: true, Identity: plugin.Identity{Software: "Editor"}, Artifact: plugin.Artifact{Path: "leased.dmg", Format: "dmg", Evidence: map[string]json.RawMessage{"macos.application": evidence, "macos.version_key": json.RawMessage(`"CFBundleVersion"`)}}, Facts: plugin.Facts{Subjects: []plugin.Subject{selected, {Kind: "app", App: &plugin.AppFacts{BundleID: "example.other", Version: "1"}}}}, Metadata: json.RawMessage(`{}`)}
	values, _, err := Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(values)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	installs := decoded["installs"].([]any)[0].(map[string]any)
	if values["version"] != "203" || installs["CFBundleIdentifier"] != "example.editor" || installs["path"] != "/Applications/Editor.app" || installs["version_comparison_key"] != "CFBundleVersion" {
		t.Fatalf("native evidence: %s", data)
	}
	if _, exists := values["supported_architectures"]; exists {
		t.Fatal("application evidence must not infer host eligibility")
	}
	request.Metadata = json.RawMessage(`{"pkginfo":{"version":"99","installs":[],"supported_architectures":["arm64"]}}`)
	values, _, err = Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	if values["version"] != "99" || len(values["installs"].([]any)) != 0 || values["supported_architectures"].([]any)[0] != "arm64" {
		t.Fatal(values)
	}
}
