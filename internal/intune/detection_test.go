package intune

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestNativeDetectionComparisons(t *testing.T) {
	msi := object{"@odata.type": "#microsoft.graph.win32LobAppProductCodeRule", "ruleType": "detection", "productCode": "{11111111-1111-4111-8111-111111111111}", "productVersionOperator": "greaterThanOrEqual", "productVersion": "2.0.0"}
	file := object{"@odata.type": "#microsoft.graph.win32LobAppFileSystemRule", "ruleType": "detection", "path": `C:\Program Files\Example`, "fileOrFolderName": "Example.exe", "operationType": "version", "operator": "greaterThanOrEqual", "comparisonValue": "2.0.0"}
	registry := object{"@odata.type": "#microsoft.graph.win32LobAppRegistryRule", "ruleType": "detection", "keyPath": `HKEY_LOCAL_MACHINE\Software\Example`, "valueName": "Version", "operationType": "version", "operator": "greaterThanOrEqual", "comparisonValue": "2.0.0"}
	script := object{"@odata.type": "#microsoft.graph.win32LobAppPowerShellScriptRule", "ruleType": "detection", "scriptContent": base64.StdEncoding.EncodeToString([]byte("Write-Output 'Installed'; exit 0")), "runAs32Bit": false}
	missingVersion := maps.Clone(msi)
	delete(missingVersion, "productVersion")
	missingComparison := maps.Clone(file)
	delete(missingComparison, "comparisonValue")
	for _, tt := range []struct {
		name  string
		rules []any
		valid bool
	}{
		{"msi accepts newer with same code", []any{msi}, true},
		{"file accepts newer at stable path", []any{file}, true},
		{"registry accepts newer at stable value", []any{registry}, true},
		{"manual rules are ANDed", []any{msi, file, registry}, true},
		{"script alternative", []any{script}, true},
		{"two product codes cannot express OR", []any{msi, maps.Clone(msi)}, false},
		{"script cannot mix manual detection", []any{script, file}, false},
		{"missing MSI version", []any{missingVersion}, false},
		{"missing file comparison", []any{missingComparison}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := string(raw(tt.rules))
			if err := validateRules(tt.rules); (err == nil) != tt.valid {
				t.Fatalf("valid=%v: %v", tt.valid, err)
			}
			if err := plugin.ValidateSchema(raw(MetadataSchema()), raw(object{"type": "win32", "rules": tt.rules})); (err == nil) != tt.valid {
				t.Fatalf("schema valid=%v: %v", tt.valid, err)
			}
			if string(raw(tt.rules)) != before {
				t.Fatal("validation changed authored native detection")
			}
		})
	}
}

func TestChangedProductCodeUpdatesDetectionInSameApp(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	req.Binding = response.Binding
	changePayload(t, &req, "new major MSI product")
	rule := desired["rules"].([]any)[0].(object)
	rule["productCode"] = "{22222222-2222-4222-8222-222222222222}"
	rule["productVersionOperator"], rule["productVersion"] = "greaterThanOrEqual", "3.0.0"
	if _, err := c.handle(t.Context(), req, configuration{}, desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.commits != 2 || !reflect.DeepEqual(fake.app["rules"], desired["rules"]) {
		t.Fatal("major upgrade did not publish the explicit new ProductCode and >= rule in the bound app")
	}
}

func TestDependenciesRequirePublishedWin32Apps(t *testing.T) {
	for _, appType := range []string{pkgType, "#microsoft.graph.windowsMobileMSI", win32Type} {
		t.Run(appType, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			c.appType = win32Type
			fake.relatedApps["dependency"] = object{"id": "dependency", "@odata.type": appType, "publishingState": "published"}
			req := fixtureRequest(t)
			req.Bindings = map[string]json.RawMessage{"runtime": raw(binding{AppID: "dependency"})}
			_, err := c.desiredRelationships(t.Context(), req, lifecycle{Dependencies: []relationshipReference{{Software: "runtime", Install: true}}}, "")
			if (err == nil) != (appType == win32Type) {
				t.Fatalf("target type %s: %v", appType, err)
			}
			if err != nil && !strings.Contains(err.Error(), "must be a published Win32 app") {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}
