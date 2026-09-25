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
	derivedValue := maps.Clone(file)
	delete(derivedValue, "value")
	derivedComparison := maps.Clone(registry)
	delete(derivedComparison, "operator")
	delete(derivedComparison, "value")
	missingSize := maps.Clone(file)
	missingSize["property"] = "size_mb"
	delete(missingSize, "value")
	missingOperator := maps.Clone(registry)
	missingOperator["property"], missingOperator["value"] = "string", "stable"
	delete(missingOperator, "operator")
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
		{"version comparison derives its value", []any{derivedValue}, true},
		{"version comparison derives its operator and value", []any{derivedComparison}, true},
		{"size comparison requires a value", []any{missingSize}, false},
		{"string comparison requires an operator", []any{missingOperator}, false},
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
	m, err = compile(plugin.ReconcileRequest[Config]{Metadata: raw(object{"type": "win32", "detection": []any{derivedComparison}})})
	if err != nil {
		t.Fatal(err)
	}
	if rule := m["rules"].([]any)[0].(object); rule["operator"] != "greaterThanOrEqual" || rule["comparisonValue"] != nil {
		t.Fatalf("derived comparison reached Graph as %+v", rule)
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

func TestVersionRulesCompareWithTheManagedVersion(t *testing.T) {
	file := object{"type": "file", "path": `C:\Program Files\Example`, "name": "Example.exe", "property": "version"}
	pinned := object{"type": "registry", "key": `HKEY_LOCAL_MACHINE\Software\Example`, "value_name": "Version", "property": "version", "operator": "equal"}
	declared := object{"type": "file", "path": `C:\Program Files\Example`, "name": "Helper.exe", "property": "version", "operator": "greater_than_or_equal", "value": "1.0"}
	msi := &plugin.MSIFacts{ProductName: "Example", Manufacturer: "Vendor", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0.0"}
	identity := plugin.Identity{Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: "example"}}
	artifact := plugin.Artifact{Path: "/leased/Example.msi", Filename: "Example.msi", Version: "2.0.1.300", Evidence: selectedMSI(msi)}
	req := plugin.ReconcileRequest[Config]{Prepared: true, Identity: identity, Metadata: raw(object{"detection": []any{file, pinned, declared}}), Artifact: artifact}
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := decodeObject(derived.Metadata)
	rules := metadata["rules"].([]any)
	want := [][2]string{{"greaterThanOrEqual", "2.0.1.300"}, {"equal", "2.0.1.300"}, {"greaterThanOrEqual", "1.0"}}
	for i, rule := range rules {
		if rule := rule.(object); rule["operator"] != want[i][0] || rule["comparisonValue"] != want[i][1] {
			t.Fatalf("rule %d reached Graph as %+v", i, rule)
		}
	}
	if origins["detection.0.value"] != "artifact.version" || origins["detection.1.value"] != "artifact.version" || origins["detection.2.value"] != "" || metadata["msiInformation"].(object)["productVersion"] != "2.0.0" {
		t.Fatalf("origins %v, MSI information %v", origins, metadata["msiInformation"])
	}
	req.Metadata = raw(object{"detection": []any{file}})
	for name, artifact := range map[string]plugin.Artifact{
		"no managed version": {Path: "/leased/setup.exe", Filename: "setup.exe"},
		"not a version":      {Path: "/leased/setup.exe", Filename: "setup.exe", Version: "v2.0"},
	} {
		req.Artifact = artifact
		if _, _, err := Derive(req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	req.Prepared, req.Artifact = false, plugin.Artifact{}
	if _, _, err := Derive(req); err != nil {
		t.Fatalf("validation without an artifact: %v", err)
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
