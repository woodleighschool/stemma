package intune

import (
	"encoding/base64"
	"slices"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestMSIDerivationRequiresSelectedFactsAndLeavesExecutionExplicit(t *testing.T) {
	req := plugin.ReconcileRequest{
		Method: "validate", Prepared: true,
		Metadata: raw(object{"derive": object{"msi": "installer"}, "displayName": "Authored name", "msiInformation": object{"publisher": "Authored publisher"}}),
		Subjects: map[string]plugin.SubjectSelector{"installer": {Kind: "msi", Path: "setup.msi"}},
		Facts: plugin.Facts{Version: 1, Subjects: []plugin.Subject{
			{Kind: "msi", Path: "setup.msi", MSI: &plugin.MSIFacts{ProductName: "Observed name", Manufacturer: "Observed publisher", ProductVersion: "2.0", ProductCode: "{11111111-1111-4111-8111-111111111111}"}},
			{Kind: "msi", Path: "helper.msi", MSI: &plugin.MSIFacts{ProductName: "Wrong MSI", ProductVersion: "9.0"}},
		}},
	}
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if m["@odata.type"] != win32Type || m["displayName"] != "Authored name" || m["publisher"] != "Observed publisher" {
		t.Fatalf("wrong selected or authored metadata: %+v", m)
	}
	info := m["msiInformation"].(object)
	if info["productVersion"] != "2.0" || info["publisher"] != "Authored publisher" || origins["msiInformation.productVersion"] == "" {
		t.Fatalf("lost native MSI overrides or provenance: %+v / %+v", info, origins)
	}
	for _, key := range []string{"installCommandLine", "uninstallCommandLine", "installExperience", "rules", "allowedArchitectures"} {
		if _, exists := m[key]; exists {
			t.Fatalf("derive guessed execution policy %s", key)
		}
	}
	req.Subjects["installer"] = plugin.SubjectSelector{Kind: "msi"}
	if _, _, err := Derive(req); err == nil {
		t.Fatal("ambiguous MSI selection was accepted")
	}
}

func TestMacDerivationKeepsExactOSAndAuthoredDetection(t *testing.T) {
	req := plugin.ReconcileRequest{
		Method: "validate", Prepared: true,
		Metadata: raw(object{"type": "pkg", "derive": object{"app": "main"}}),
		Subjects: map[string]plugin.SubjectSelector{"main": {BundleID: "org.example.app"}},
		Facts:    plugin.Facts{Subjects: []plugin.Subject{{Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "2.0", Name: "Example", MinimumOS: "14.1"}}}},
	}
	if _, _, err := Derive(req); err == nil {
		t.Fatal("rounded down an unsupported minimum OS")
	}
	req.Metadata = raw(object{"type": "pkg", "derive": object{"app": "main"}, "minimumSupportedOperatingSystem": object{"v15_0": true}, "primaryBundleVersion": "3.0"})
	derived, _, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if m["includedApps"].([]any)[0].(object)["bundleVersion"] != "3.0" || selectedOS(m["minimumSupportedOperatingSystem"]) != "v15_0" {
		t.Fatalf("authored version or OS was replaced: %+v", m)
	}
	for _, version := range []string{"14", "14.0", "14.0.0", "26.0"} {
		if _, err := minimumOS(version); err != nil {
			t.Fatalf("supported exact OS %s: %v", version, err)
		}
	}
}

func TestStaticValidationDeclaresReferencesWithoutContentOrBindings(t *testing.T) {
	req := plugin.ReconcileRequest{Method: "validate", Config: raw(object{"token": "synthetic"}),
		Subjects: map[string]plugin.SubjectSelector{"installer": {Kind: "msi"}},
		Metadata: raw(object{
			"derive":       object{"msi": "installer"},
			"dependencies": []any{object{"software": "runtime", "auto_install": true}},
			"supersedes":   []any{object{"software": "previous", "uninstall_previous": false}},
			"retention":    object{"keep": 2},
		}),
	}
	response, err := Handle(t.Context(), req)
	if err != nil || !slices.Equal(response.Requires, []string{"previous", "runtime"}) {
		t.Fatalf("static dependency discovery: %+v, %v", response, err)
	}
	req.Prepared = true
	if _, err := Handle(t.Context(), req); err == nil {
		t.Fatal("runtime validation accepted missing selected MSI facts")
	}
}

func TestIntuneAuthoringSchemaAndProviderAgree(t *testing.T) {
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
		{"references", object{"type": "win32", "dependencies": []any{object{"software": "runtime", "auto_install": true}}, "retention": object{"keep": 1}}, true},
		{"missing type", object{"displayName": "Example"}, false},
		{"bad retention", object{"type": "win32", "retention": object{"keep": 0}}, false},
		{"mac dependency", object{"type": "pkg", "dependencies": []any{}}, false},
		{"missing relationship policy", object{"type": "win32", "dependencies": []any{object{"software": "runtime"}}}, false},
		{"script context", object{"type": "win32", "rules": []any{object{"@odata.type": "#microsoft.graph.win32LobAppPowerShellScriptRule", "ruleType": "detection", "scriptContent": script, "runAsAccount": "system"}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := raw(test.metadata)
			if err := plugin.ValidateSchema(raw(MetadataSchema()), metadata); (err == nil) != test.valid {
				t.Fatalf("schema validity differs: %v", err)
			}
			_, err := Handle(t.Context(), plugin.ReconcileRequest{Method: "validate", Config: raw(object{"token": "synthetic"}), Metadata: metadata})
			if (err == nil) != test.valid {
				t.Fatalf("provider validity differs: %v", err)
			}
		})
	}
}
