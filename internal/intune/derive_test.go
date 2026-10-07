package intune

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

// macRequest describes a prepared macOS artifact, its selected application and
// the effective minimum OS the engine sends with it.
func macRequest(metadata object, selected *plugin.Subject, subjects ...plugin.Subject) plugin.ReconcileRequest[Config] {
	artifact := plugin.Artifact{Facts: plugin.Facts{Version: plugin.FactsVersion, Subjects: subjects}}
	if selected != nil {
		evidence, _ := json.Marshal(selected)
		artifact.Evidence = map[string]json.RawMessage{"macos.application": evidence}
	}
	identity := plugin.Identity{Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "example"}}
	return plugin.ReconcileRequest[Config]{Method: "validate", Identity: identity, Prepared: true, Metadata: raw(metadata), Artifact: artifact, MinimumOS: &plugin.MinimumOS{Version: "13.0", Origin: "software.minimum_os"}}
}

func TestMacMinimumOSMapsToItsRelease(t *testing.T) {
	app := plugin.Subject{ID: "Payload/Example.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "2.0", Name: "Example"}}
	req := macRequest(object{"type": "pkg"}, &app, plugin.Subject{ID: ".", Path: ".", Kind: "container"}, app)
	for _, test := range []struct{ version, field, origin string }{
		{"14", "v14_0", "app.minimum_os"},
		{"14.0.0", "v14_0", "app.minimum_os"},
		{"14.2", "v14_0", "app.minimum_os 14.2 → v14_0"},
		{"10.13", "v10_13", "app.minimum_os"},
		{"10.13.6", "v10_13", "app.minimum_os 10.13.6 → v10_13"},
		{"26.1", "v26_0", "app.minimum_os 26.1 → v26_0"},
	} {
		req.MinimumOS = &plugin.MinimumOS{Version: test.version, Origin: "app.minimum_os"}
		derived, origins, err := Derive(req)
		if err != nil {
			t.Fatalf("%s: %v", test.version, err)
		}
		m, err := decodeObject(derived.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		if selectedOS(m["minimumSupportedOperatingSystem"]) != test.field || origins["minimum_os"] != test.origin {
			t.Fatalf("%s: %v from %q, want %s from %q", test.version, m["minimumSupportedOperatingSystem"], origins["minimum_os"], test.field, test.origin)
		}
	}
	// A release without its own setting fails rather than moving to another release.
	for _, version := range []string{"10.6", "16.0", "27.0"} {
		req.MinimumOS = &plugin.MinimumOS{Version: version, Origin: "software.minimum_os"}
		if _, _, err := Derive(req); err == nil {
			t.Fatalf("macOS %s was mapped to another release", version)
		}
	}
	req.MinimumOS = nil
	if _, _, err := Derive(req); err == nil || !strings.Contains(err.Error(), "minimum_os") {
		t.Fatalf("created an app without a minimum OS: %v", err)
	}
	req.MinimumOS = &plugin.MinimumOS{Version: "14.0", Origin: "app.minimum_os"}
	req.Metadata = raw(object{"type": "pkg", "minimum_os": "12.0"})
	if _, _, err := Derive(req); err == nil {
		t.Fatal("declared minimum OS replaced the software's requirement")
	}
}

func TestApplicationDiskImageDerivesADmgApp(t *testing.T) {
	app := plugin.Subject{ID: "WoodSweep.app", Path: "WoodSweep.app", Parent: ".", Kind: "app", InstalledPath: "/Applications/WoodSweep.app", App: &plugin.AppFacts{BundleID: "org.example.woodsweep", Version: "1.2.3", Name: "WoodSweep", MinimumOS: "14.0"}}
	req := macRequest(object{"type": "dmg"}, &app, plugin.Subject{ID: ".", Path: ".", Kind: "container"}, app)
	req.Artifact.Path, req.Artifact.Filename, req.Artifact.Format, req.Artifact.SHA256 = "leased.dmg", "woodsweep-1.2.3.dmg", "dmg", strings.Repeat("a", 64)
	req.MinimumOS = &plugin.MinimumOS{Version: "14.0", Origin: "app.minimum_os"}
	derived, _, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeObject(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	included := m["includedApps"].([]any)[0].(object)
	if m["@odata.type"] != dmgType || included["bundleId"] != "org.example.woodsweep" || included["bundleVersion"] != "1.2.3" || selectedOS(m["minimumSupportedOperatingSystem"]) != "v14_0" {
		t.Fatalf("derived app: %+v", m)
	}
	if _, named := m["displayName"]; named {
		t.Fatalf("display name was derived from the application: %+v", m)
	}
	if identity, err := identifyArtifact(t.Context(), req.Artifact, dmgType); err != nil || !identity.raw {
		t.Fatalf("disk image was not published as it is: %+v: %v", identity, err)
	}
}

// An installer's other applications don't decide whether it installed.
func TestDiskImageDetectsTheSelectedApplication(t *testing.T) {
	layout := plugin.Subject{ID: "SketchUp 2026/LayOut.app", Parent: ".", Kind: "app", App: &plugin.AppFacts{BundleID: "com.trimble.layout", Version: "26.0", MinimumOS: "13.0"}}
	sketchup := plugin.Subject{ID: "SketchUp 2026/SketchUp.app", Parent: ".", Kind: "app", InstalledPath: "/Applications/SketchUp 2026/SketchUp.app", App: &plugin.AppFacts{BundleID: "com.trimble.sketchup", Version: "26.1", Name: "SketchUp", MinimumOS: "13.0"}}
	derived, origins, err := Derive(macRequest(object{"type": "dmg"}, &sketchup, plugin.Subject{ID: ".", Path: ".", Kind: "container"}, layout, sketchup))
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeObject(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	apps := m["includedApps"].([]any)
	if len(apps) != 1 || apps[0].(object)["bundleId"] != "com.trimble.sketchup" || m["primaryBundleId"] != "com.trimble.sketchup" || m["primaryBundleVersion"] != "26.1" || origins["included_apps"] != "app.bundle_id" {
		t.Fatalf("included apps: %+v / %+v", m, origins)
	}
}

func TestPackageDetectionUsesTheSelectedApplicationOrReceipts(t *testing.T) {
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container", Installer: &plugin.InstallerFacts{MinimumOS: "11.0"}}
	receipt := plugin.Subject{ID: "PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter", Version: "1.4.0", HasPayload: true}}
	scripts := plugin.Subject{ID: "scripts.pkg/PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter.scripts", Version: "1.4.0"}}
	helper := plugin.Subject{ID: "Payload/Helper.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Library/Application Support/Example/Helper.app", App: &plugin.AppFacts{BundleID: "org.example.helper", Version: "1.4.0"}}
	updater := plugin.Subject{ID: "Payload/Updater.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Library/Application Support/Example/Updater.app", App: &plugin.AppFacts{BundleID: "org.example.updater", Version: "9.0"}}
	req := macRequest(object{"display_name": "Exporter"}, &helper, root, receipt, scripts, helper, updater)
	req.MinimumOS = &plugin.MinimumOS{Version: "11.0", Origin: "installer.minimum_os"}
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeObject(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	apps := m["includedApps"].([]any)
	if len(apps) != 1 || apps[0].(object)["bundleId"] != "org.example.helper" || m["primaryBundleVersion"] != "1.4.0" || selectedOS(m["minimumSupportedOperatingSystem"]) != "v11_0" || origins["included_apps"] != "app.bundle_id" || m["displayName"] != "Exporter" {
		t.Fatalf("application detection: %+v / %+v", m, origins)
	}
	// Without a selection, the receipts detect the PKG, not the applications it carries.
	delete(req.Artifact.Evidence, "macos.application")
	for _, subjects := range [][]plugin.Subject{{root, receipt, receipt}, {root, scripts}, {root, receipt, helper, updater}} {
		req.Artifact.Facts.Subjects = subjects
		derived, origins, err = Derive(req)
		if err != nil {
			t.Fatal(err)
		}
		m, err = decodeObject(derived.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		apps = m["includedApps"].([]any)
		component := subjects[1].Package
		if len(apps) != 1 || apps[0].(object)["bundleId"] != component.Identifier || apps[0].(object)["bundleVersion"] != "1.4.0" || m["primaryBundleId"] != component.Identifier || m["primaryBundleVersion"] != "1.4.0" || origins["included_apps"] != "installer.receipts" {
			t.Fatalf("receipt detection: %+v / %+v", m, origins)
		}
	}
	req.Metadata = raw(object{"type": "dmg"})
	if _, _, err := Derive(req); err == nil {
		t.Fatal("DMG detection accepted package receipts")
	}
}

// A selected application that can't detect the installation fails instead of
// falling back to other applications or receipts.
func TestSelectedApplicationDetectionFailsWithoutFallback(t *testing.T) {
	receipt := plugin.Subject{ID: "PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter", Version: "1.4", HasPayload: true}}
	other := plugin.Subject{ID: "Payload/Other.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Other.app", App: &plugin.AppFacts{BundleID: "org.example.other", Version: "1.4"}}
	app := plugin.Subject{ID: "Payload/Exporter.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Exporter.app", App: &plugin.AppFacts{BundleID: "org.example.exporter.app", Version: "1.4"}}
	for _, test := range []struct {
		name, appType, rejection string
		change                   func(*plugin.Subject)
	}{
		{"no bundle identifier", "pkg", "CFBundleIdentifier", func(app *plugin.Subject) { app.App.BundleID = "" }},
		{"no version", "pkg", "CFBundleShortVersionString", func(app *plugin.Subject) { app.App.Version = "" }},
		{"DMG outside Applications", "dmg", "does not install under /Applications", func(app *plugin.Subject) { app.InstalledPath = "/Library/Exporter/Exporter.app" }},
		{"DMG without install path", "dmg", "does not install under /Applications", func(app *plugin.Subject) { app.InstalledPath = "" }},
		{"outside Applications", "lob", "does not install under /Applications", func(app *plugin.Subject) { app.InstalledPath = "/Library/Exporter/Exporter.app" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := app
			facts := *app.App
			selected.App = &facts
			test.change(&selected)
			req := lobRequest(object{"type": test.appType}, &selected, plugin.Subject{ID: ".", Path: ".", Kind: "container"}, receipt, other, selected)
			if _, _, err := Derive(req); err == nil || !strings.Contains(err.Error(), test.rejection) {
				t.Fatalf("got %v, want %s", err, test.rejection)
			}
		})
	}
}

// lobRequest describes a signed flat PKG prepared for a line-of-business app.
func lobRequest(metadata object, selected *plugin.Subject, subjects ...plugin.Subject) plugin.ReconcileRequest[Config] {
	req := macRequest(metadata, selected, subjects...)
	req.Artifact.Path, req.Artifact.Filename, req.Artifact.Format, req.Artifact.Size = "leased.pkg", "exporter-1.4.0.pkg", "pkg", 4096
	if req.Artifact.Evidence == nil {
		req.Artifact.Evidence = map[string]json.RawMessage{}
	}
	req.Artifact.Evidence["signatures"] = json.RawMessage(`[{"subject":{"path":"."},"state":"signed","signer":"apple:developer-id:ABCDE12345","authority":"Developer ID Installer","verifier":"stemma.signature/4"}]`)
	return req
}

func TestLineOfBusinessAppListsChildApps(t *testing.T) {
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container"}
	receipt := plugin.Subject{ID: "PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter", Version: "1.4.0", HasPayload: true}}
	app := plugin.Subject{ID: "Payload/Exporter.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Exporter.app", App: &plugin.AppFacts{BundleID: "org.example.exporter.app", Version: "1.4", Build: "140"}}
	derived, origins, err := Derive(lobRequest(object{"type": "lob", "install_as_managed": true}, &app, root, receipt, app))
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeObject(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	child := m["childApps"].([]any)[0].(object)
	if m["@odata.type"] != lobType || m["bundleId"] != "org.example.exporter.app" || m["buildNumber"] != "1.4" || m["versionNumber"] != "140" || child["bundleId"] != "org.example.exporter.app" || child["buildNumber"] != "1.4" || child["versionNumber"] != "140" || origins["included_apps"] != "app.bundle_id" || m["installAsManaged"] != true {
		t.Fatalf("application detection: %+v / %+v", m, origins)
	}
	if _, exists := m["includedApps"]; exists {
		t.Fatalf("line-of-business app kept PKG detection: %+v", m)
	}
	if derived, _, err = Derive(lobRequest(object{"type": "lob", "included_apps": []any{object{"id": "org.example.declared", "version": "2.0"}}}, &app, root, receipt, app)); err != nil {
		t.Fatal(err)
	}
	if m, err = decodeObject(derived.Metadata); err != nil {
		t.Fatal(err)
	}
	if child := m["childApps"].([]any)[0].(object); len(m["childApps"].([]any)) != 1 || child["bundleId"] != "org.example.declared" || child["buildNumber"] != "2.0" || m["bundleId"] != "org.example.declared" {
		t.Fatalf("declared child apps: %+v", m)
	}
}

// Intune accepts a line-of-business app whose included apps name a package
// receipt, and it then fails on devices with "The app state is unknown". An
// identifier the PKG doesn't show may be an application a script installs.
func TestLineOfBusinessIncludedAppsAreApplications(t *testing.T) {
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container"}
	tools := plugin.Subject{ID: "tools.pkg/PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter", Version: "1.4.0", HasPayload: true}}
	helper := plugin.Subject{ID: "tools.pkg/Payload/Helper.app", Parent: "tools.pkg/PackageInfo", Kind: "app", InstalledPath: "/Library/Exporter/Helper.app", App: &plugin.AppFacts{BundleID: "org.example.helper", Version: "1.4"}}
	bundle := plugin.Subject{ID: "app.pkg/PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter.app", Version: "1.4", HasPayload: true}}
	app := plugin.Subject{ID: "app.pkg/Payload/Exporter.app", Parent: "app.pkg/PackageInfo", Kind: "app", InstalledPath: "/Applications/Exporter.app", App: &plugin.AppFacts{BundleID: "org.example.exporter.app", Version: "1.4"}}
	for _, test := range []struct{ name, id, rejection string }{
		{"application that is also a receipt", "org.example.exporter.app", ""},
		{"identifier the PKG doesn't show", "org.example.scripted", ""},
		{"package receipt", "org.example.exporter", "package receipt"},
		{"application outside Applications", "org.example.helper", "not installed under /Applications"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := lobRequest(object{"type": "lob", "included_apps": []any{object{"id": test.id, "version": "1.4"}}}, nil, root, tools, helper, bundle, app)
			_, _, err := Derive(req)
			if test.rejection == "" && err != nil || test.rejection != "" && (err == nil || !strings.Contains(err.Error(), test.rejection)) {
				t.Fatalf("included app %s: %v", test.id, err)
			}
		})
	}
}

func TestDeclaredMacDetectionDoesNotRequireInventory(t *testing.T) {
	for _, appType := range []string{"pkg", "dmg", "lob"} {
		t.Run(appType, func(t *testing.T) {
			req := macRequest(object{"type": appType, "display_name": "Example", "description": "Example app", "publisher": "Example", "included_apps": []any{object{"id": "org.example.app", "version": "2.0"}}}, nil)
			derived, _, err := Derive(req)
			if err != nil {
				t.Fatal(err)
			}
			m, err := decodeObject(derived.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateCreation(m); err != nil {
				t.Fatalf("explicit detection did not supply primary identity: %v", err)
			}
			req.Metadata = raw(object{"type": appType})
			if _, _, err := Derive(req); err == nil || !strings.Contains(err.Error(), "included_apps") {
				t.Fatalf("missing prepared inventory silently preserved earlier detection: %v", err)
			}
		})
	}
}

func TestLineOfBusinessUploadRequirements(t *testing.T) {
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container"}
	receipt := plugin.Subject{ID: "PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter", Version: "1.4.0", HasPayload: true}}
	helper := plugin.Subject{ID: "helper.pkg/PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.helper", Version: "1.4.0", HasPayload: true}}
	scripts := plugin.Subject{ID: "PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.scripts", Version: "1.4.0"}}
	app := plugin.Subject{ID: "Payload/Exporter.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Exporter.app", App: &plugin.AppFacts{BundleID: "org.example.exporter.app", Version: "1.4"}}
	elsewhere := app
	elsewhere.InstalledPath = "/Library/Exporter/Exporter.app"
	managed := object{"type": "lob", "install_as_managed": true}
	for _, test := range []struct {
		name      string
		metadata  object
		selected  *plugin.Subject
		subjects  []plugin.Subject
		rejection string
		change    func(*plugin.ReconcileRequest[Config])
	}{
		{"disk image", object{"type": "lob"}, nil, []plugin.Subject{root, receipt}, "requires a flat PKG", func(req *plugin.ReconcileRequest[Config]) {
			req.Artifact.Filename, req.Artifact.Format = "exporter.dmg", "dmg"
		}},
		{"unsigned", object{"type": "lob"}, nil, []plugin.Subject{root, receipt}, "Developer ID Installer signature", func(req *plugin.ReconcileRequest[Config]) { delete(req.Artifact.Evidence, "signatures") }},
		{"oversized", object{"type": "lob"}, nil, []plugin.Subject{root, receipt}, "at most 2 GiB", func(req *plugin.ReconcileRequest[Config]) { req.Artifact.Size = lobLimit + 1 }},
		{"no payload", object{"type": "lob"}, nil, []plugin.Subject{root, scripts}, "requires a payload", nil},
		{"no application", object{"type": "lob"}, nil, []plugin.Subject{root, receipt}, "application under /Applications", nil},
		{"declared detection without an application", object{"type": "lob", "included_apps": []any{object{"id": "org.example.exporter", "version": "1.4.0"}}}, nil, []plugin.Subject{root, receipt}, "application under /Applications", nil},
		{"application outside Applications", object{"type": "lob"}, &elsewhere, []plugin.Subject{root, receipt, elsewhere}, "application under /Applications", nil},
		{"managed with two components", managed, &app, []plugin.Subject{root, receipt, helper, app}, "install_as_managed requires", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := lobRequest(test.metadata, test.selected, test.subjects...)
			if test.change != nil {
				test.change(&req)
			}
			if _, _, err := Derive(req); err == nil || !strings.Contains(err.Error(), test.rejection) {
				t.Fatalf("got %v, want %s", err, test.rejection)
			}
		})
	}
	windows := plugin.ReconcileRequest[Config]{Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: "example"}}, Metadata: raw(object{"type": "lob"})}
	if _, err := compile(windows); err == nil {
		t.Fatal("Windows software became a macOS line-of-business app")
	}
}

func TestStaticValidationAcceptsReferencesWithoutContent(t *testing.T) {
	req := plugin.ReconcileRequest[Config]{Method: "validate", Config: Config{GraphURL: "https://graph.microsoft.com", Token: "synthetic"},
		Metadata: raw(object{
			"type":         "win32",
			"dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "auto_install": true}},
			"supersedes":   []any{object{"resource": object{"kind": "WindowsSoftware", "name": "previous"}, "uninstall_previous": false}},
		}),
	}
	response, err := Handle(t.Context(), req)
	if err != nil {
		t.Fatalf("static relationship validation: %+v, %v", response, err)
	}
}

// Graph returns architecture flags in its own order; a declared set in any
// order must compile to that spelling or every plan would report a change.
func TestArchitecturesCompileToGraphFlags(t *testing.T) {
	req := plugin.ReconcileRequest[Config]{Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: "example"}}, Metadata: raw(object{"architectures": []any{"arm64", "x64"}})}
	m, err := compile(req)
	if err != nil || m["allowedArchitectures"] != "x64,arm64" {
		t.Fatalf("allowedArchitectures = %#v, %v", m["allowedArchitectures"], err)
	}
}

func TestIntuneConfigurationSchemaAndProviderAgree(t *testing.T) {
	registry := object{"type": "registry", "key": `HKEY_LOCAL_MACHINE\Software\Example`, "value_name": "Version", "property": "version", "operator": "greater_than_or_equal", "value": "2.0"}
	for _, test := range []struct {
		name     string
		kind     string
		metadata object
		valid    bool
	}{
		{"type defaults from the kind", "WindowsSoftware", object{"display_name": "Example"}, true},
		{"null type", "WindowsSoftware", object{"type": nil}, false},
		{"MSI information", "WindowsSoftware", object{"msi": object{"product_version": "2.0", "package_type": "per_machine"}}, true},
		{"setup entrypoint", "WindowsSoftware", object{"content": object{"setup_file": "bin/setup.exe"}}, false},
		{"registry", "WindowsSoftware", object{"detection": []any{registry}}, true},
		{"script", "WindowsSoftware", object{"detection": []any{object{"type": "script", "script": "Write-Output 'installed'\nexit 0\n", "run_as_32bit": false}}}, true},
		{"assignment", "WindowsSoftware", object{"assignments": []any{object{"intent": "required", "group": "88208ef5-07a0-4627-ad8f-e9c1c0ff5f15"}, object{"intent": "available", "all_users": true}}}, true},
		{"assignment with two targets", "WindowsSoftware", object{"assignments": []any{object{"intent": "required", "group": "group-1", "all_devices": true}}}, false},
		{"Windows assignment notifications", "WindowsSoftware", object{"type": "win32", "assignments": []any{object{"intent": "required", "all_devices": true, "notifications": "hide_all"}}}, true},
		{"Mac assignment notifications", "MacSoftware", object{"type": "pkg", "assignments": []any{object{"intent": "required", "all_devices": true, "notifications": "hide_all"}}}, false},
		{"Mac LOB assignment notifications", "MacSoftware", object{"type": "lob", "assignments": []any{object{"intent": "required", "all_devices": true, "notifications": "hide_all"}}}, false},
		{"references", "WindowsSoftware", object{"dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "auto_install": true}}}, true},
		{"external relationship", "WindowsSoftware", object{"dependencies": []any{object{"app_id": "existing-app", "auto_install": true}}}, true},
		{"ambiguous relationship", "WindowsSoftware", object{"dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "app_id": "existing-app", "auto_install": true}}}, false},
		{"old software syntax", "WindowsSoftware", object{"dependencies": []any{object{"software": "runtime", "auto_install": true}}}, false},
		{"missing reference kind", "WindowsSoftware", object{"dependencies": []any{object{"resource": object{"name": "runtime"}, "auto_install": true}}}, false},
		{"publication output", "WindowsSoftware", object{"dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime", "output": "installer"}, "auto_install": true}}}, false},
		{"Graph property name", "WindowsSoftware", object{"displayName": "Example"}, false},
		// Graph cannot delete content versions, so Intune has no retention.
		{"retention", "WindowsSoftware", object{"retention": object{"keep": 1}}, false},
		{"mac dependency", "MacSoftware", object{"type": "pkg", "dependencies": []any{}}, false},
		{"missing relationship policy", "WindowsSoftware", object{"dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}}}}, false},
		{"script context", "WindowsSoftware", object{"detection": []any{object{"type": "script", "script": "exit 0", "run_as": "system"}}}, false},
		{"named subject derivation", "WindowsSoftware", object{"derive": object{"msi": "installer"}}, false},
		{"declared minimum OS", "MacSoftware", object{"minimum_os": "14.0"}, false},
		{"line-of-business app", "MacSoftware", object{"type": "lob", "install_as_managed": true}, true},
		{"architectures", "WindowsSoftware", object{"architectures": []any{"x64", "arm64"}}, true},
		{"cleared architectures", "WindowsSoftware", object{"architectures": nil}, true},
		{"single architecture", "WindowsSoftware", object{"architecture": "x64"}, false},
		{"empty architectures", "WindowsSoftware", object{"architectures": []any{}}, false},
		{"repeated architecture", "WindowsSoftware", object{"architectures": []any{"x64", "x64"}}, false},
		{"MSI properties", "WindowsSoftware", object{"msi_properties": object{"PORTAL": "vpn.example.com", "zConfig": "AU2_EnableAutoUpdate=true"}}, true},
		{"MSI properties and install command", "WindowsSoftware", object{"msi_properties": object{"PORTAL": "vpn"}, "install_command": "setup.exe /S"}, false},
		{"MSI property name", "WindowsSoftware", object{"msi_properties": object{"1PORTAL": "vpn"}}, false},
		{"MSI property name with NUL", "WindowsSoftware", object{"msi_properties": object{"POR\x00TAL": "vpn"}}, false},
		{"MSI property value line break", "WindowsSoftware", object{"msi_properties": object{"PORTAL": "vpn\r\nother"}}, false},
		{"Mac MSI properties", "MacSoftware", object{"type": "pkg", "msi_properties": object{"PORTAL": "vpn"}}, false},
		{"managed PKG app", "MacSoftware", object{"type": "pkg", "install_as_managed": true}, false},
		{"Windows line-of-business app", "WindowsSoftware", object{"type": "lob", "install_command": "setup.exe"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := raw(test.metadata)
			if err := plugin.ValidateSchema(raw(MetadataSchema()), metadata); (err == nil) != test.valid {
				t.Fatalf("schema validity differs: %v", err)
			}
			identity := plugin.Identity{Resource: plugin.ResourceReference{Kind: test.kind, Name: "example"}}
			_, err := Handle(t.Context(), plugin.ReconcileRequest[Config]{Method: "validate", Identity: identity, Config: Config{GraphURL: "https://graph.microsoft.com", Token: "synthetic"}, Metadata: metadata})
			if (err == nil) != test.valid {
				t.Fatalf("provider validity differs: %v", err)
			}
		})
	}
}

func TestLOBRequiresThePublishedRootInstallerObservation(t *testing.T) {
	for _, evidence := range []string{
		`[{"subject":{"path":"."},"state":"unsigned","verifier":"stemma.signature/4"}]`,
		`[{"input":"vendor","subject":{"path":"."},"state":"signed","signer":"apple:developer-id:ABCDE12345","authority":"Developer ID Installer","verifier":"stemma.signature/4"}]`,
		`[{"subject":{"path":"Companion.app"},"state":"signed","signer":"apple:developer-id:ABCDE12345","authority":"Developer ID Application","verifier":"stemma.signature/4"}]`,
		`[{"subject":{"path":"PackageInfo"},"state":"signed","signer":"apple:developer-id:ABCDE12345","authority":"Developer ID Installer","verifier":"stemma.signature/4"}]`,
	} {
		artifact := plugin.Artifact{Path: "/leased/upload.pkg", Filename: "upload.pkg", Evidence: map[string]json.RawMessage{"signatures": json.RawMessage(evidence)}}
		if err := validateLOB(artifact, nil, false); err == nil || !strings.Contains(err.Error(), "published PKG root") {
			t.Fatalf("accepted wrong signature evidence: %v", err)
		}
	}
	req := lobRequest(object{"type": "pkg"}, nil, plugin.Subject{ID: "receipt", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.autopkg", Version: "1.0"}})
	req.Artifact.Evidence = map[string]json.RawMessage{"signatures": json.RawMessage(`[{"subject":{"path":"."},"state":"unsigned","verifier":"stemma.signature/4"}]`)}
	if _, _, err := Derive(req); err != nil {
		t.Fatalf("ordinary PKG rejected unsigned root: %v", err)
	}
}
