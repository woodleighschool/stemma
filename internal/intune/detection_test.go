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

func TestDetectionRules(t *testing.T) {
	msi := object{"type": "msi", "product_code": "{11111111-1111-4111-8111-111111111111}", "operator": "greater_than_or_equal", "product_version": "2.0.0"}
	file := object{"type": "file", "path": `C:\Program Files\Example`, "name": "Example.exe", "property": "version", "operator": "greater_than_or_equal", "value": "2.0.0"}
	registry := object{"type": "registry", "key": `HKEY_LOCAL_MACHINE\Software\Example`, "value_name": "Version", "property": "version", "operator": "greater_than_or_equal", "value": "2.0.0"}
	exists := object{"type": "file", "path": `C:\Program Files\Example`, "name": "Example.exe", "property": "exists"}
	script := object{"type": "script", "script": "Write-Output 'Installed'; exit 0", "run_as_32bit": false}
	missingVersion := maps.Clone(msi)
	delete(missingVersion, "product_version")
	missingComparison := maps.Clone(file)
	delete(missingComparison, "value")
	comparedExistence := maps.Clone(exists)
	comparedExistence["operator"] = "equal"
	for _, tt := range []struct {
		name  string
		rules []any
		valid bool
	}{
		{"msi accepts newer with same code", []any{msi}, true},
		{"file accepts newer at stable path", []any{file}, true},
		{"registry accepts newer at stable value", []any{registry}, true},
		{"existence takes no comparison", []any{exists}, true},
		{"manual rules are ANDed", []any{msi, file, registry}, true},
		{"script alternative", []any{script}, true},
		{"two product codes cannot express OR", []any{msi, maps.Clone(msi)}, false},
		{"script cannot mix manual detection", []any{script, file}, false},
		{"missing MSI version", []any{missingVersion}, false},
		{"missing file comparison", []any{missingComparison}, false},
		{"compared existence", []any{comparedExistence}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata := raw(object{"type": "win32", "detection": tt.rules})
			_, err := compile(plugin.ReconcileRequest[Config]{Metadata: metadata})
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v: %v", tt.valid, err)
			}
			if err := plugin.ValidateSchema(raw(MetadataSchema()), metadata); (err == nil) != tt.valid {
				t.Fatalf("schema valid=%v: %v", tt.valid, err)
			}
		})
	}
	m, err := compile(plugin.ReconcileRequest[Config]{Metadata: raw(object{"type": "win32", "detection": []any{file}})})
	if err != nil {
		t.Fatal(err)
	}
	if rule := m["rules"].([]any)[0].(object); rule["fileOrFolderName"] != "Example.exe" || rule["operationType"] != "version" || rule["operator"] != "greaterThanOrEqual" || rule["comparisonValue"] != "2.0.0" || rule["ruleType"] != "detection" {
		t.Fatalf("file rule reached Graph as %+v", rule)
	}
	m, err = compile(plugin.ReconcileRequest[Config]{Metadata: raw(object{"type": "win32", "detection": []any{script}})})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := base64.StdEncoding.DecodeString(text(m["rules"].([]any)[0].(object)["scriptContent"]))
	if string(content) != script["script"] {
		t.Fatalf("script reached Graph as %q", content)
	}
}

func TestChangedProductCodeUpdatesDetectionInSameApp(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := compile(req)
	delete(desired, "assignments")
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	changePayload(t, &req, "new major MSI product")
	rule := desired["rules"].([]any)[0].(object)
	rule["productCode"] = "{22222222-2222-4222-8222-222222222222}"
	rule["productVersionOperator"], rule["productVersion"] = "greaterThanOrEqual", "3.0.0"
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.commits != 2 || !reflect.DeepEqual(fake.app["rules"], desired["rules"]) {
		t.Fatal("major upgrade did not publish the explicit new ProductCode and >= rule in the same app")
	}
}

func TestDependenciesRequirePublishedWin32Apps(t *testing.T) {
	for _, appType := range []string{pkgType, "#microsoft.graph.windowsMobileMSI", win32Type} {
		t.Run(appType, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			c.appType = win32Type
			fake.relatedApps["dependency"] = object{"id": "dependency", "@odata.type": appType, "publishingState": "published"}
			req := fixtureRequest(t)
			req.Peers = map[string]json.RawMessage{"stemma/v1alpha1/WindowsSoftware/runtime": raw(object{"app_id": "dependency"})}
			_, err := c.desiredRelationships(t.Context(), req, &tenantApps{client: c}, lifecycle{Dependencies: []relationshipReference{{Resource: &plugin.ResourceReference{Kind: "WindowsSoftware", Name: "runtime"}, Install: true}}}, "")
			if (err == nil) != (appType == win32Type) {
				t.Fatalf("target type %s: %v", appType, err)
			}
			if err != nil && !strings.Contains(err.Error(), "must be a published Win32 app") {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}
