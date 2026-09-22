package intune

import (
	"encoding/base64"
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
	return plugin.ReconcileRequest[Config]{Method: "validate", Prepared: true, Metadata: raw(metadata), Artifact: artifact, MinimumOS: &plugin.MinimumOS{Version: "13.0", Origin: "software.minimum_os"}}
}

func TestMacMinimumOSMapsToItsRelease(t *testing.T) {
	app := plugin.Subject{ID: "Payload/Example.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "2.0", Name: "Example"}}
	req := macRequest(object{"type": "pkg"}, &app, plugin.Subject{ID: ".", Path: ".", Kind: "container"}, app)
	for _, test := range []struct{ version, field, origin string }{
		{"14", "v14_0", "app.minimum_os"},
		{"14.0.0", "v14_0", "app.minimum_os"},
		{"14.2", "v14_0", "app.minimum_os 14.2 -> v14_0"},
		{"10.13", "v10_13", "app.minimum_os"},
		{"10.13.6", "v10_13", "app.minimum_os 10.13.6 -> v10_13"},
		{"26.1", "v26_0", "app.minimum_os 26.1 -> v26_0"},
	} {
		req.MinimumOS = &plugin.MinimumOS{Version: test.version, Origin: "app.minimum_os"}
		derived, origins, err := Derive(req)
		if err != nil {
			t.Fatalf("%s: %v", test.version, err)
		}
		m, err := validateMetadata(derived.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		if selectedOS(m["minimumSupportedOperatingSystem"]) != test.field || origins["minimumSupportedOperatingSystem"] != test.origin {
			t.Fatalf("%s: %v from %q, want %s from %q", test.version, m["minimumSupportedOperatingSystem"], origins["minimumSupportedOperatingSystem"], test.field, test.origin)
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
	req.Metadata = raw(object{"type": "pkg", "minimumSupportedOperatingSystem": object{"v12_0": true}})
	if _, _, err := Derive(req); err == nil {
		t.Fatal("declared minimum OS replaced the software's requirement")
	}
	req.Metadata = raw(object{"type": "pkg", "primaryBundleVersion": "3.0"})
	derived, _, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if m["includedApps"].([]any)[0].(object)["bundleVersion"] != "3.0" {
		t.Fatalf("declared version was replaced: %+v", m)
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
	m, err := validateMetadata(derived.Metadata)
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
	if identity, err := identifyArtifact(t.Context(), req.Artifact, dmgType, ""); err != nil || !identity.raw {
		t.Fatalf("disk image was not published as it is: %+v: %v", identity, err)
	}
}

func TestDiskImageIncludesEveryApplicationSelectedFirst(t *testing.T) {
	layout := plugin.Subject{ID: "SketchUp 2026/LayOut.app", Parent: ".", Kind: "app", App: &plugin.AppFacts{BundleID: "com.trimble.layout", Version: "26.0", MinimumOS: "13.0"}}
	sketchup := plugin.Subject{ID: "SketchUp 2026/SketchUp.app", Parent: ".", Kind: "app", InstalledPath: "/Applications/SketchUp 2026/SketchUp.app", App: &plugin.AppFacts{BundleID: "com.trimble.sketchup", Version: "26.1", Name: "SketchUp", MinimumOS: "13.0"}}
	derived, origins, err := Derive(macRequest(object{"type": "dmg"}, &sketchup, plugin.Subject{ID: ".", Path: ".", Kind: "container"}, layout, sketchup))
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	apps := m["includedApps"].([]any)
	if len(apps) != 2 || apps[0].(object)["bundleId"] != "com.trimble.sketchup" || apps[1].(object)["bundleId"] != "com.trimble.layout" || m["primaryBundleId"] != "com.trimble.sketchup" || m["primaryBundleVersion"] != "26.1" || origins["includedApps"] != "installer.apps" {
		t.Fatalf("included apps: %+v / %+v", m, origins)
	}
}

func TestPackageWithoutApplicationsIsDetectedByReceipts(t *testing.T) {
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container", Installer: &plugin.InstallerFacts{MinimumOS: "11.0"}}
	receipt := plugin.Subject{ID: "PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter", Version: "1.4.0", HasPayload: true}}
	scripts := plugin.Subject{ID: "scripts.pkg/PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.exporter.scripts", Version: "1.4.0"}}
	helper := plugin.Subject{ID: "Payload/Helper.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Library/Application Support/Example/Helper.app", App: &plugin.AppFacts{BundleID: "org.example.helper", Version: "1.4.0"}}
	req := macRequest(object{"type": "pkg", "displayName": "Exporter"}, nil, root, receipt, scripts, helper)
	req.MinimumOS = &plugin.MinimumOS{Version: "11.0", Origin: "installer.minimum_os"}
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	apps := m["includedApps"].([]any)
	if len(apps) != 1 || apps[0].(object)["bundleId"] != "org.example.exporter" || m["primaryBundleVersion"] != "1.4.0" || selectedOS(m["minimumSupportedOperatingSystem"]) != "v11_0" || origins["includedApps"] != "installer.receipts" || m["displayName"] != "Exporter" {
		t.Fatalf("receipt detection: %+v / %+v", m, origins)
	}
}

func TestStaticValidationAcceptsReferencesWithoutContent(t *testing.T) {
	req := plugin.ReconcileRequest[Config]{Method: "validate", Config: Config{GraphURL: "https://graph.microsoft.com/v1.0", Token: "synthetic"},
		Metadata: raw(object{
			"type":         "win32",
			"dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "auto_install": true}},
			"supersedes":   []any{object{"resource": object{"kind": "WindowsSoftware", "name": "previous"}, "uninstall_previous": false}},
			"retention":    object{"keep": 2},
		}),
	}
	response, err := Handle(t.Context(), req)
	if err != nil {
		t.Fatalf("static relationship validation: %+v, %v", response, err)
	}
}

func TestIntuneConfigurationSchemaAndProviderAgree(t *testing.T) {
	script := base64.StdEncoding.EncodeToString([]byte("Write-Output 'installed'\nexit 0\n"))
	for _, test := range []struct {
		name     string
		metadata object
		valid    bool
	}{
		{"native MSI", object{"@odata.type": win32Type, "msiInformation": object{"productVersion": "2.0", "packageType": "perMachine"}}, true},
		{"setup entrypoint", object{"type": "win32", "content": object{"setup_file": "bin/setup.exe"}}, true},
		{"unknown content option", object{"type": "win32", "content": object{"setup_file": "setup.exe", "run": true}}, false},
		{"missing setup entrypoint", object{"type": "win32", "content": object{}}, false},
		{"mac setup tree", object{"type": "pkg", "content": object{"setup_file": "setup.exe"}}, false},
		{"registry", object{"type": "win32", "rules": []any{object{"@odata.type": "#microsoft.graph.win32LobAppRegistryRule", "ruleType": "detection", "keyPath": `HKEY_LOCAL_MACHINE\Software\Example`, "valueName": "Version", "operationType": "version", "operator": "greaterThanOrEqual", "comparisonValue": "2.0"}}}, true},
		{"script", object{"type": "win32", "rules": []any{object{"@odata.type": "#microsoft.graph.win32LobAppPowerShellScriptRule", "ruleType": "detection", "scriptContent": script, "runAs32Bit": false}}}, true},
		{"references", object{"type": "win32", "dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "auto_install": true}}, "retention": object{"keep": 1}}, true},
		{"external relationship", object{"type": "win32", "dependencies": []any{object{"app_id": "existing-app", "auto_install": true}}}, true},
		{"ambiguous relationship", object{"type": "win32", "dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "app_id": "existing-app", "auto_install": true}}}, false},
		{"old software syntax", object{"type": "win32", "dependencies": []any{object{"software": "runtime", "auto_install": true}}}, false},
		{"missing reference kind", object{"type": "win32", "dependencies": []any{object{"resource": object{"name": "runtime"}, "auto_install": true}}}, false},
		{"publication output", object{"type": "win32", "dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime", "output": "installer"}, "auto_install": true}}}, false},
		{"missing type", object{"displayName": "Example"}, false},
		{"named subject derivation", object{"type": "win32", "derive": object{"msi": "installer"}}, false},
		{"declared minimum OS", object{"type": "pkg", "minimumSupportedOperatingSystem": object{"v14_0": true}}, false},
		{"bad retention", object{"type": "win32", "retention": object{"keep": 0}}, false},
		{"mac dependency", object{"type": "pkg", "dependencies": []any{}}, false},
		{"missing relationship policy", object{"type": "win32", "dependencies": []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}}}}, false},
		{"script context", object{"type": "win32", "rules": []any{object{"@odata.type": "#microsoft.graph.win32LobAppPowerShellScriptRule", "ruleType": "detection", "scriptContent": script, "runAsAccount": "system"}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := raw(test.metadata)
			if err := plugin.ValidateSchema(raw(MetadataSchema()), metadata); (err == nil) != test.valid {
				t.Fatalf("schema validity differs: %v", err)
			}
			_, err := Handle(t.Context(), plugin.ReconcileRequest[Config]{Method: "validate", Config: Config{GraphURL: "https://graph.microsoft.com/v1.0", Token: "synthetic"}, Metadata: metadata})
			if (err == nil) != test.valid {
				t.Fatalf("provider validity differs: %v", err)
			}
		})
	}
}
