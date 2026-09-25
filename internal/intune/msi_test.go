package intune

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestSelectedMSIDefaultsAndNativeOverrides(t *testing.T) {
	msi := &plugin.MSIFacts{ProductName: "Example", Manufacturer: "Vendor", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0.0", UpgradeCode: "{33333333-3333-4333-8333-333333333333}"}
	req := plugin.ReconcileRequest[Config]{Prepared: true, Metadata: raw(object{"type": "win32"}), Artifact: plugin.Artifact{Filename: "Example.msi", Facts: plugin.Facts{Subjects: []plugin.Subject{{Kind: "msi", MSI: msi}}}}}
	for _, code := range []string{msi.ProductCode, "{22222222-2222-4222-8222-222222222222}"} {
		msi.ProductCode = code
		req.Artifact.Evidence = selectedMSI(msi)
		derived, origins, err := Derive(req)
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := decodeObject(derived.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		rule := metadata["rules"].([]any)[0].(object)
		if metadata["installCommandLine"] != `msiexec /i "Example.msi" /qn /norestart` || metadata["uninstallCommandLine"] != `msiexec /x "`+code+`" /qn /norestart` || rule["productCode"] != code || rule["productVersionOperator"] != "greaterThanOrEqual" || rule["productVersion"] != "2.0.0" || origins["detection"] != "windows.installer" {
			t.Fatalf("incorrect MSI defaults: %+v", metadata)
		}
		if metadata["installExperience"] != nil || metadata["returnCodes"] != nil || metadata["allowedArchitectures"] != nil {
			t.Fatal("MSI facts guessed device deployment policy")
		}
		if _, named := metadata["displayName"]; named || metadata["publisher"] != nil {
			t.Fatalf("MSI facts supplied descriptive fields: %+v", metadata)
		}
	}
	req.Artifact.Evidence = nil
	if derived, origins, err := Derive(req); err != nil || len(origins) != 0 {
		t.Fatalf("destination selected an MSI itself: %s, %v", derived.Metadata, err)
	}
	req.Artifact.Evidence = selectedMSI(msi)
	req.Metadata = raw(object{"type": "win32", "display_name": "Declared", "install_command": "custom install", "uninstall_command": "custom remove", "detection": []any{}})
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := decodeObject(derived.Metadata)
	if metadata["displayName"] != "Declared" || metadata["installCommandLine"] != "custom install" || len(metadata["rules"].([]any)) != 0 || origins["detection"] != "" {
		t.Fatal("MSI defaults overwrote declared native fields")
	}
	req.Artifact.Filename = "setup.exe"
	req.Metadata = raw(object{"type": "win32"})
	derived, origins, err = Derive(req)
	if err != nil || len(origins) != 0 {
		t.Fatalf("EXE acquired guessed policy: %s, %v", derived.Metadata, err)
	}
}

func TestMSIPropertiesExtendTheDerivedInstallCommand(t *testing.T) {
	msi := &plugin.MSIFacts{ProductName: "Example", Manufacturer: "Vendor", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0.0"}
	properties := object{"PORTAL": "vpn.example.com", "zConfig": `AU2_EnableAutoUpdate=true;Title="Example"`, "CONNECTMETHOD": "", "INSTALLDIR": `C:\Program Files\Vendor`}
	req := plugin.ReconcileRequest[Config]{Prepared: true, Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: "example"}}, Metadata: raw(object{"msi_properties": properties}), Artifact: plugin.Artifact{Path: "/leased/Example.msi", Filename: "Example.msi", Evidence: selectedMSI(msi)}}
	derived, _, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := decodeObject(derived.Metadata)
	want := `msiexec /i "Example.msi" /qn /norestart CONNECTMETHOD="" INSTALLDIR="C:\Program Files\Vendor" PORTAL="vpn.example.com" zConfig="AU2_EnableAutoUpdate=true;Title=""Example"""`
	if metadata["installCommandLine"] != want || metadata["msi_properties"] != nil {
		t.Fatalf("install command: %v", metadata["installCommandLine"])
	}
	req.Artifact = plugin.Artifact{Path: "/leased/setup.exe", Filename: "setup.exe"}
	if _, _, err := Derive(req); err == nil {
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
		if _, err := compile(req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// selectedMSI records the setup MSI a WindowsSoftware preparation selected.
func selectedMSI(msi *plugin.MSIFacts) map[string]json.RawMessage {
	evidence, _ := json.Marshal(plugin.Subject{Kind: "msi", MSI: msi})
	return map[string]json.RawMessage{"windows.installer": evidence}
}
