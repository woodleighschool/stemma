package munki_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/plugin"
)

func selected(artifact plugin.Artifact, app plugin.Subject, versionKey string) plugin.Artifact {
	artifact.Evidence = map[string]json.RawMessage{}
	artifact.Evidence["macos.application"], _ = json.Marshal(app)
	if versionKey != "" {
		artifact.Evidence["macos.version_key"], _ = json.Marshal(versionKey)
	}
	return artifact
}

func TestDestinationDerivesSelectedDMGAppAndNativeOverrides(t *testing.T) {
	editor := plugin.Subject{ID: "Editor.app", Kind: "app", Path: "Editor.app", InstalledPath: "/Applications/Editor.app", App: &plugin.AppFacts{BundleID: "example.editor", Name: "Editor", Version: "2.3", Build: "203", MinimumOS: "13.0"}}
	other := plugin.Subject{ID: "Other.app", Kind: "app", Path: "Other.app", App: &plugin.AppFacts{BundleID: "example.other", Version: "1"}}
	request := plugin.ReconcileRequest[json.RawMessage]{Prepared: true, Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "Editor"}}, Artifact: selected(plugin.Artifact{Path: "leased.dmg", Format: "dmg", Version: "3.0", Facts: plugin.Facts{Subjects: []plugin.Subject{editor, other}}}, editor, "CFBundleVersion"), Metadata: json.RawMessage(`{"pkginfo":{"description":null,"blocking_applications":[]}}`)}
	derived, err := munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(derived.Values)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	installs := decoded["installs"].([]any)[0].(map[string]any)
	if installs["path"] != "/Applications/Editor.app" || installs["CFBundleIdentifier"] != "example.editor" || installs["CFBundleVersion"] != "203" || installs["CFBundleShortVersionString"] != "2.3" || installs["version_comparison_key"] != "CFBundleVersion" || decoded["version"] != "3.0" {
		t.Fatalf("derived detection: %#v", decoded)
	}
	if _, exists := derived.Values["supported_architectures"]; exists {
		t.Fatal("application evidence inferred host eligibility")
	}
	if _, exists := decoded["description"]; !exists || derived.Origins["pkginfo.description"] != "explicit" {
		t.Fatal("explicit null lost")
	}
	request.Metadata = json.RawMessage(`{"pkginfo":{"version":"9","items_to_copy":[{"source_item":"Editor.app","destination_path":"/Applications/Managed","destination_item":"Renamed.app"}],"installs":[],"supported_architectures":["arm64"]}}`)
	if _, err := munki.Derive(request); err == nil {
		t.Fatal("copy destination disagreeing with the installed path was accepted")
	}
	editor.InstalledPath = "/Applications/Managed/Renamed.app"
	request.Artifact = selected(request.Artifact, editor, "")
	derived, err = munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	if derived.Values["version"] != "9" || len(derived.Values["installs"].([]any)) != 0 || derived.Values["supported_architectures"].([]any)[0] != "arm64" {
		t.Fatal("native overrides did not win")
	}
	request.Artifact.Evidence = nil
	if derived, err = munki.Derive(request); err != nil || derived.Values["version"] != "9" {
		t.Fatalf("destination selected an application itself: %v, %v", derived.Values, err)
	}
}

func TestDerivedApplicationUsesNativeDetectionKeys(t *testing.T) {
	app := plugin.Subject{ID: "Editor.app", Kind: "app", Path: "Editor.app", InstalledPath: "/Applications/Editor.app", App: &plugin.AppFacts{BundleID: "example.editor", Name: "Editor", Version: "banana", Build: "2349", MinimumOS: "13.2"}}
	request := plugin.ReconcileRequest[json.RawMessage]{Prepared: true, Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "Editor"}}, Artifact: selected(plugin.Artifact{Path: "leased.dmg", Format: "dmg", Version: "2349", Facts: plugin.Facts{Subjects: []plugin.Subject{app}}}, app, ""), MinimumOS: &plugin.MinimumOS{Version: "14.0", Origin: "software.minimum_os"}, Metadata: json.RawMessage(`{}`)}
	derived, err := munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(derived.Values)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	installs := decoded["installs"].([]any)[0].(map[string]any)
	if decoded["version"] != "2349" || installs["version_comparison_key"] != "CFBundleVersion" || installs["minosversion"] != "13.2" || installs["minimum_os_version"] != nil || decoded["minimum_os_version"] != "14.0" {
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
	request := plugin.ReconcileRequest[json.RawMessage]{Prepared: true, Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "Suite"}}, Artifact: plugin.Artifact{Path: "leased.pkg", Format: "pkg", Version: "9.4", Facts: facts}, MinimumOS: &plugin.MinimumOS{Version: "14.2", Origin: "installer.minimum_os"}, Metadata: json.RawMessage(`{}`)}
	derived, err := munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(derived.Values)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if decoded["version"] != "9.4" || len(decoded["receipts"].([]any)) != 2 || decoded["installed_size"] != float64(14) || decoded["RestartAction"] != "RequireLogout" || decoded["minimum_os_version"] != "14.2" || decoded["uninstallable"] != true || decoded["uninstall_method"] != "removepackages" {
		t.Fatalf("derived package metadata: %s", data)
	}
	if derived.Origins["pkginfo.version"] != "artifact.version" || derived.Origins["pkginfo.minimum_os_version"] != "installer.minimum_os" {
		t.Fatalf("derived origins: %v", derived.Origins)
	}
	request.Metadata = json.RawMessage(`{"pkginfo":{"uninstallable":false}}`)
	if derived, err = munki.Derive(request); err != nil {
		t.Fatal(err)
	}
	if _, exists := derived.Values["uninstall_method"]; exists {
		t.Fatal("derived removal overrode declared uninstallable")
	}
	request.Artifact.Version = ""
	if _, err := munki.Derive(request); err == nil {
		t.Fatal("a package without a managed version was accepted")
	}
}

func TestPKGAppRequiresKnownEndpoint(t *testing.T) {
	app := plugin.Subject{ID: "Payload/App.app", Kind: "app", Path: "Payload/App.app", App: &plugin.AppFacts{BundleID: "example.app", Version: "1"}}
	request := plugin.ReconcileRequest[json.RawMessage]{Prepared: true, Artifact: selected(plugin.Artifact{Path: "vendor.pkg", Format: "pkg", Version: "1", Facts: plugin.Facts{Subjects: []plugin.Subject{app}}}, app, ""), Metadata: json.RawMessage(`{}`)}
	if _, err := munki.Derive(request); err == nil {
		t.Fatal("archive path became an endpoint path")
	}
	app.InstalledPath = "/Applications/App.app"
	request.Artifact = selected(request.Artifact, app, "")
	if _, err := munki.Derive(request); err != nil {
		t.Fatal(err)
	}
	request.Prepared = false
	request.Artifact = plugin.Artifact{}
	if _, err := munki.Derive(request); err != nil {
		t.Fatalf("static validation required artifact facts: %v", err)
	}
}

func TestDerivationOwnsAttemptedFieldsWithoutEvidence(t *testing.T) {
	request := plugin.ReconcileRequest[json.RawMessage]{Prepared: true, Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "Editor"}}, Artifact: plugin.Artifact{Path: "leased.pkg", Format: "pkg", Version: "1"}, Metadata: json.RawMessage(`{}`)}
	derived, err := munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"minimum_os_version", "RestartAction", "installed_size", "installs", "items_to_copy", "receipts", "uninstall_method", "uninstallable"}; !slices.Equal(derived.Cleared, want) {
		t.Fatalf("package without evidence cleared %v, want %v", derived.Cleared, want)
	}
	request.Artifact.Facts = plugin.Facts{Subjects: []plugin.Subject{{Kind: "package", Package: &plugin.PackageFacts{Identifier: "example.editor", Version: "1", HasPayload: true, InstalledSize: 20}}}}
	request.Metadata = json.RawMessage(`{"pkginfo":{"installs":[],"uninstallable":false}}`)
	derived, err = munki.Derive(request)
	if err != nil {
		t.Fatal(err)
	}
	// An item declared not uninstallable has no removal method.
	if want := []string{"minimum_os_version", "RestartAction", "items_to_copy", "uninstall_method"}; !slices.Equal(derived.Cleared, want) {
		t.Fatalf("declared and evidenced fields were cleared: %v", derived.Cleared)
	}
	// Each removal field is settled on its own: the method follows the
	// installer, and an item with a method is uninstallable.
	for metadata, want := range map[string]string{
		`{"pkginfo":{"uninstallable":true}}`:                                                   "removepackages",
		`{"pkginfo":{"uninstall_method":"uninstall_script","uninstall_script":"#!/bin/sh\n"}}`: "uninstall_script",
	} {
		request.Metadata = json.RawMessage(metadata)
		if derived, err = munki.Derive(request); err != nil || derived.Values["uninstallable"] != true || derived.Values["uninstall_method"] != want {
			t.Fatalf("%s derived removal %v %v: %v", metadata, derived.Values["uninstallable"], derived.Values["uninstall_method"], err)
		}
	}
	// An installer-free item owns only the minimum, which the software declares.
	request.Metadata = json.RawMessage(`{"pkginfo":{"installer_type":"nopkg"}}`)
	request.Artifact = plugin.Artifact{Version: "1"}
	if derived, err = munki.Derive(request); err != nil || !slices.Equal(derived.Cleared, []string{"minimum_os_version"}) {
		t.Fatalf("installer-free item owns derived fields: %v %v", derived.Cleared, err)
	}
	request.MinimumOS = &plugin.MinimumOS{Version: "15.0", Origin: "software.minimum_os"}
	if derived, err = munki.Derive(request); err != nil || len(derived.Cleared) != 0 || derived.Values["minimum_os_version"] != "15.0" {
		t.Fatalf("installer-free item ignored the declared minimum: %v %v", derived.Values, err)
	}
}

func TestCatalogCannotDeclareMinimumOS(t *testing.T) {
	schema, err := json.Marshal(munki.DestinationSchema())
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateSchema(schema, json.RawMessage(`{"pkginfo":{"minimum_os_version":"11.0"}}`)); err == nil {
		t.Fatal("catalog pkginfo lowered the derived minimum")
	}
}
