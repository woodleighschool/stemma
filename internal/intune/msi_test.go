package intune

import (
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestSelectedMSIDefaultsAndNativeOverrides(t *testing.T) {
	msi := &plugin.MSIFacts{ProductName: "Example", Manufacturer: "Vendor", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0.0", UpgradeCode: "{33333333-3333-4333-8333-333333333333}"}
	req := plugin.ReconcileRequest{Prepared: true, Metadata: raw(object{"type": "win32"}), Artifact: plugin.Artifact{Filename: "Example.msi", Facts: plugin.Facts{Subjects: []plugin.Subject{{Kind: "msi", MSI: msi}}}}}
	for _, code := range []string{msi.ProductCode, "{22222222-2222-4222-8222-222222222222}"} {
		msi.ProductCode = code
		derived, origins, err := Derive(req)
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := validateMetadata(derived.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		rule := metadata["rules"].([]any)[0].(object)
		if metadata["installCommandLine"] != `msiexec /i "Example.msi" /qn /norestart` || metadata["uninstallCommandLine"] != `msiexec /x "`+code+`" /qn /norestart` || rule["productCode"] != code || rule["productVersionOperator"] != "greaterThanOrEqual" || rule["productVersion"] != "2.0.0" || origins["rules"] != "windows.installer" {
			t.Fatalf("incorrect MSI defaults: %+v", metadata)
		}
		if metadata["installExperience"] != nil || metadata["returnCodes"] != nil || metadata["allowedArchitectures"] != nil {
			t.Fatal("MSI facts guessed device deployment policy")
		}
	}
	req.Metadata = raw(object{"type": "win32", "displayName": "Declared", "installCommandLine": "custom install", "uninstallCommandLine": "custom remove", "rules": []any{}})
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := decodeObject(derived.Metadata)
	if metadata["displayName"] != "Declared" || metadata["installCommandLine"] != "custom install" || len(metadata["rules"].([]any)) != 0 || origins["rules"] != "" {
		t.Fatal("MSI defaults overwrote declared native fields")
	}
	req.Artifact.Filename = "setup.exe"
	req.Metadata = raw(object{"type": "win32"})
	derived, origins, err = Derive(req)
	if err != nil || len(origins) != 0 {
		t.Fatalf("EXE acquired guessed policy: %s, %v", derived.Metadata, err)
	}
}
