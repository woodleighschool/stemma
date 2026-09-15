package munki_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/plugin"
)

func TestDestinationDerivesSelectedDMGAppAndNativeOverrides(t *testing.T) {
	request := plugin.ReconcileRequest{Prepared: true, Identity: plugin.Identity{Software: "Editor"}, Artifact: plugin.Artifact{Path: "leased.dmg", Format: "dmg"}, Subjects: map[string]plugin.SubjectSelector{"app": {Kind: "app", BundleID: "example.editor"}}, Facts: plugin.Facts{Subjects: []plugin.Subject{{Kind: "app", Path: "Editor.app", App: &plugin.AppFacts{BundleID: "example.editor", Name: "Editor", Version: "2.3", Build: "203", MinimumOS: "13.0"}}, {Kind: "app", Path: "Other.app", App: &plugin.AppFacts{BundleID: "example.other", Version: "1"}}}}, Metadata: json.RawMessage(`{"derive":{"app":{"subject":"app","version_key":"CFBundleVersion"}},"pkginfo":{"description":null,"blocking_applications":[]}}`)}
	values, origins, err := munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(values)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	installs := decoded["installs"].([]any)[0].(map[string]any)
	if installs["path"] != "/Applications/Editor.app" || installs["CFBundleVersion"] != "203" || installs["CFBundleShortVersionString"] != "2.3" || installs["version_comparison_key"] != "CFBundleVersion" || decoded["version"] != "203" {
		t.Fatalf("derived detection: %#v", decoded)
	}
	if _, exists := decoded["description"]; !exists || origins["pkginfo.description"] != "authored" {
		t.Fatal("explicit null lost")
	}
	request.Metadata = json.RawMessage(`{"derive":{"app":{"subject":"app"}},"unmanaged":["pkginfo.minimum_os_version"]}`)
	values, _, err = munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := values["minimum_os_version"]; exists {
		t.Fatal("unmanaged field was derived")
	}
	request.Metadata = json.RawMessage(`{"unmanaged":["pkginfo.minimum_os_version"],"pkginfo":{"minimum_os_version":"12.0"}}`)
	if _, _, err := munki.Derive(request); err == nil {
		t.Fatal("authored and unmanaged field overlap accepted")
	}
	request.Metadata = json.RawMessage(`{"derive":{"app":{"subject":"app"}},"pkginfo":{"version":"9","items_to_copy":[{"source_item":"Editor.app","destination_path":"/Applications/Managed","destination_item":"Renamed.app"}],"installs":[]}}`)
	values, _, err = munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	if values["version"] != "9" || len(values["installs"].([]any)) != 0 {
		t.Fatal("native overrides did not win")
	}
}

func TestDerivedApplicationUsesNativeDetectionKeys(t *testing.T) {
	app := plugin.Subject{Kind: "app", Path: "Editor.app", App: &plugin.AppFacts{BundleID: "example.editor", Name: "Editor", Version: "banana", Build: "2349", MinimumOS: "13.2"}}
	request := plugin.ReconcileRequest{Prepared: true, Identity: plugin.Identity{Software: "Editor"}, Artifact: plugin.Artifact{Path: "leased.dmg", Format: "dmg"}, Facts: plugin.Facts{Subjects: []plugin.Subject{app}}, Metadata: json.RawMessage(`{}`)}
	values, _, err := munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(values)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	installs := decoded["installs"].([]any)[0].(map[string]any)
	if decoded["version"] != "2349" || installs["version_comparison_key"] != "CFBundleVersion" || installs["minosversion"] != "13.2" || installs["minimum_os_version"] != nil || decoded["minimum_os_version"] != "13.2" {
		t.Fatalf("derived detection: %s", data)
	}
	if decoded["uninstallable"] != true || decoded["uninstall_method"] != "remove_copied_items" {
		t.Fatalf("derived removal: %s", data)
	}
}

func TestDerivedPackageMetadata(t *testing.T) {
	facts := plugin.Facts{Subjects: []plugin.Subject{
		{ID: ".", Kind: "container", Installer: &plugin.InstallerFacts{Version: "9.4", MinimumOS: "14.2", RestartAction: "RequireLogout"}},
		{ID: "Alpha.pkg/PackageInfo", Kind: "package", Package: &plugin.PackageFacts{Identifier: "example.alpha", Version: "4.5.6", InstalledSize: 4, HasPayload: true}},
		{ID: "Beta.pkg/PackageInfo", Kind: "package", Package: &plugin.PackageFacts{Identifier: "example.beta", Version: "7.8.9", InstalledSize: 10, HasPayload: true}},
		{ID: "Scripts.pkg/PackageInfo", Kind: "package", Package: &plugin.PackageFacts{Identifier: "example.scripts", Version: "1.0"}},
	}}
	request := plugin.ReconcileRequest{Prepared: true, Identity: plugin.Identity{Software: "Suite"}, Artifact: plugin.Artifact{Path: "leased.pkg", Format: "pkg"}, Facts: facts, Metadata: json.RawMessage(`{}`)}
	values, _, err := munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(values)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if decoded["version"] != "9.4" || len(decoded["receipts"].([]any)) != 2 || decoded["installed_size"] != float64(14) || decoded["RestartAction"] != "RequireLogout" || decoded["minimum_os_version"] != "14.2" || decoded["uninstallable"] != true || decoded["uninstall_method"] != "removepackages" {
		t.Fatalf("derived package metadata: %s", data)
	}
	request.Metadata = json.RawMessage(`{"pkginfo":{"uninstallable":false}}`)
	if values, _, err = munki.Derive(request); err != nil {
		t.Fatal(err)
	}
	if _, exists := values["uninstall_method"]; exists {
		t.Fatal("derived removal overrode authored uninstallable")
	}
	request.Metadata = json.RawMessage(`{}`)
	for _, test := range []struct{ app, want string }{{"15.0", "15.0"}, {"13.0", "14.2"}} {
		request.Facts.Subjects = slices.Concat(facts.Subjects, []plugin.Subject{{ID: "Suite.app", Kind: "app", InstalledPath: "/Applications/Suite.app", App: &plugin.AppFacts{BundleID: "example.suite", Version: "9.4.1", MinimumOS: test.app}}})
		values, _, err := munki.Derive(request)
		if err != nil {
			t.Fatal(err)
		}
		if values["minimum_os_version"] != test.want {
			t.Fatalf("application minimum %s: minimum_os_version = %v, want %s", test.app, values["minimum_os_version"], test.want)
		}
	}
}

func TestPKGAppRequiresKnownEndpointWhenExplicitlySelected(t *testing.T) {
	request := plugin.ReconcileRequest{Prepared: true, Artifact: plugin.Artifact{Path: "vendor.pkg", Format: "pkg"}, Subjects: map[string]plugin.SubjectSelector{"app": {Kind: "app", BundleID: "example.app"}}, Facts: plugin.Facts{Subjects: []plugin.Subject{{Kind: "app", Path: "Payload/App.app", App: &plugin.AppFacts{BundleID: "example.app", Version: "1"}}}}, Metadata: json.RawMessage(`{"derive":{"app":{"subject":"app"}}}`)}
	if _, _, err := munki.Derive(request); err == nil {
		t.Fatal("archive path became an endpoint path")
	}
	request.Metadata = json.RawMessage(`{"derive":{"app":{"subject":"app","installed_path":"/Applications/App.app"}}}`)
	if _, _, err := munki.Derive(request); err != nil {
		t.Fatal(err)
	}
	request.Prepared = false
	request.Artifact = plugin.Artifact{}
	request.Facts = plugin.Facts{}
	if _, _, err := munki.Derive(request); err != nil {
		t.Fatalf("static validation required artifact facts: %v", err)
	}
}
