package intune

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestSelectedMSIDefaultsAndNativeOverrides(t *testing.T) {
	msi := &plugin.MSIFacts{ProductName: "Example", Manufacturer: "Vendor", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0.0", UpgradeCode: "{33333333-3333-4333-8333-333333333333}"}
	req := plugin.ReconcileRequest[Config]{Prepared: true, Metadata: raw(object{"type": "win32"}), Artifact: plugin.Artifact{Filename: "Example.msi", Facts: plugin.Facts{Subjects: []plugin.Subject{{Kind: "msi", MSI: msi}}}}}
	for _, code := range []string{msi.ProductCode, "{22222222-2222-4222-8222-222222222222}"} {
		msi.ProductCode = code
		req.Artifact.Evidence = selectedMSI(msi)
		metadata, origins, err := derive(req)
		if err != nil {
			t.Fatal(err)
		}
		rule := metadata["detection"].([]any)[0].(object)
		if metadata["install_command"] != `msiexec /i "Example.msi" /qn /norestart` || metadata["uninstall_command"] != `msiexec /x "`+code+`" /qn /norestart` || rule["product_code"] != code || rule["operator"] != "greater_than_or_equal" || rule["product_version"] != "2.0.0" || origins["detection"] != "windows.installer" {
			t.Fatalf("incorrect MSI defaults: %+v", metadata)
		}
		if metadata["install_experience"] != nil || metadata["return_codes"] != nil || metadata["architectures"] != nil {
			t.Fatal("MSI facts guessed device deployment policy")
		}
		if _, named := metadata["display_name"]; named || metadata["publisher"] != nil {
			t.Fatalf("MSI facts supplied descriptive fields: %+v", metadata)
		}
	}
	req.Artifact.Evidence = nil
	if metadata, origins, err := derive(req); err != nil || len(origins) != 0 {
		t.Fatalf("destination selected an MSI itself: %s, %v", raw(metadata), err)
	}
	req.Artifact.Evidence = selectedMSI(msi)
	req.Metadata = raw(object{"type": "win32", "display_name": "Declared", "install_command": "custom install", "uninstall_command": "custom remove", "detection": []any{}})
	metadata, origins, err := derive(req)
	if err != nil {
		t.Fatal(err)
	}
	if metadata["display_name"] != "Declared" || metadata["install_command"] != "custom install" || len(metadata["detection"].([]any)) != 0 || origins["detection"] != "" {
		t.Fatal("MSI defaults overwrote declared native fields")
	}
	req.Artifact.Filename = "setup.exe"
	req.Metadata = raw(object{"type": "win32"})
	metadata, origins, err = derive(req)
	if err != nil || len(origins) != 0 {
		t.Fatalf("EXE acquired guessed policy: %s, %v", raw(metadata), err)
	}
}

func TestMSIPropertiesExtendTheDerivedInstallCommand(t *testing.T) {
	msi := &plugin.MSIFacts{ProductName: "Example", Manufacturer: "Vendor", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0.0"}
	properties := object{"PORTAL": "vpn.example.com", "zConfig": `AU2_EnableAutoUpdate=true;Title="Example"`, "CONNECTMETHOD": "", "INSTALLDIR": `C:\Program Files\Vendor`}
	req := plugin.ReconcileRequest[Config]{Prepared: true, Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: "example"}}, Metadata: raw(object{"msi_properties": properties}), Artifact: plugin.Artifact{SHA256: strings.Repeat("a", 64), Filename: "Example.msi", Evidence: selectedMSI(msi)}}
	metadata, _, err := derive(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `msiexec /i "Example.msi" /qn /norestart CONNECTMETHOD="" INSTALLDIR="C:\Program Files\Vendor" PORTAL="vpn.example.com" zConfig="AU2_EnableAutoUpdate=true;Title=""Example"""`
	if metadata["install_command"] != want || metadata["msi_properties"] != nil {
		t.Fatalf("install command: %v", metadata["install_command"])
	}
	req.Artifact = plugin.Artifact{SHA256: strings.Repeat("a", 64), Filename: "setup.exe"}
	if _, _, err := derive(req); err == nil {
		t.Fatal("accepted msi_properties for an EXE setup file")
	}
	for name, metadata := range map[string]object{
		"install command":        {"msi_properties": object{"PORTAL": "vpn"}, "install_command": "setup.exe /S"},
		"property name":          {"msi_properties": object{"1PORTAL": "vpn"}},
		"name with a line break": {"msi_properties": object{"PORTAL\n": "vpn"}},
		"name with NUL":          {"msi_properties": object{"POR\x00TAL": "vpn"}},
		"multi-line value":       {"msi_properties": object{"PORTAL": "vpn\nother"}},
		"carriage return value":  {"msi_properties": object{"PORTAL": "vpn\rother"}},
		"value with NUL":         {"msi_properties": object{"PORTAL": "vpn\x00"}},
		"empty properties":       {"msi_properties": object{}},
	} {
		req.Metadata = raw(metadata)
		if _, err := compileRequest(req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// selectedMSI records the setup MSI a WindowsSoftware preparation selected.
func selectedMSI(msi *plugin.MSIFacts) map[string]json.RawMessage {
	evidence, _ := json.Marshal(plugin.Subject{Kind: "msi", MSI: msi})
	return map[string]json.RawMessage{"windows.installer": evidence}
}
