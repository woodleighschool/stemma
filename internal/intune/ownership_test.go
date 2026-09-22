package intune

import (
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestDerivedFieldWithoutArtifactValueIsClearedOrMustBeSet(t *testing.T) {
	// An MSI need not declare an UpgradeCode or a manufacturer. Graph clears the
	// first with null and requires the second.
	msi := &plugin.MSIFacts{ProductName: "Example", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0"}
	installer := plugin.Artifact{Filename: "Example.msi", Facts: plugin.Facts{Subjects: []plugin.Subject{{Kind: "msi", MSI: msi}}}, Evidence: selectedMSI(msi)}
	app := plugin.Subject{ID: "Payload/Example.app", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "2.0", MinimumOS: "14.0"}}
	selected := macRequest(nil, &app, app).Artifact
	for _, test := range []struct {
		name     string
		metadata object
		artifact plugin.Artifact
		missing  string
		check    func(object) bool
	}{
		{name: "setup MSI", metadata: object{"type": "win32"}, artifact: installer, missing: "msi.publisher"},
		{name: "setup MSI declared", metadata: object{"type": "win32", "msi": object{"publisher": "Vendor"}}, artifact: installer, check: func(m object) bool {
			info := m["msiInformation"].(object)
			cleared, owned := info["upgradeCode"]
			return owned && cleared == nil && info["publisher"] == "Vendor" && info["productVersion"] == "2.0"
		}},
		{name: "application", metadata: object{"type": "pkg"}, artifact: selected, check: func(m object) bool {
			_, named := m["displayName"]
			return !named && m["primaryBundleId"] == "org.example.app"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := plugin.ReconcileRequest[Config]{
				Prepared: true, Config: Config{GraphURL: "https://graph.microsoft.com/v1.0", Token: "synthetic"}, Metadata: raw(test.metadata), Artifact: test.artifact,
				MinimumOS: &plugin.MinimumOS{Version: "14.0", Origin: "app.minimum_os"},
			}
			// The rule reads only this declaration and artifact, so it holds for every
			// method, before the tenant is consulted and whether or not the app exists.
			for _, method := range []string{"validate", "plan", "apply"} {
				req.Method = method
				if test.missing != "" {
					if _, err := Handle(t.Context(), req); err == nil || !strings.Contains(err.Error(), test.missing) || !strings.Contains(err.Error(), "set each field") {
						t.Fatalf("%s silently omitted %s: %v", method, test.missing, err)
					}
					continue
				}
				derived, _, err := Derive(req)
				if err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				if m, err := decodeObject(derived.Metadata); err != nil || !test.check(m) {
					t.Fatalf("%s derived %s: %v", method, derived.Metadata, err)
				}
			}
		})
	}
}

func TestClearedDerivedFieldReplacesAnEarlierInstallersValue(t *testing.T) {
	published := publication{identity: strings.Repeat("a", 64)}
	current := object{"notes": withMarker("", published), "msiInformation": object{"productCode": "product-code", "upgradeCode": "earlier-upgrade-code"}}
	desired := object{"msiInformation": object{"productCode": "product-code", "upgradeCode": nil}}
	patch, changes := metadataPatch(current, desired, published)
	info, _ := patch["msiInformation"].(object)
	if value, sent := info["upgradeCode"]; !sent || value != nil || len(changes) != 1 {
		t.Fatalf("stale UpgradeCode was not cleared: %s", raw(patch))
	}
	current["msiInformation"].(object)["upgradeCode"] = nil
	if patch, _ := metadataPatch(current, desired, published); len(patch) != 0 {
		t.Fatalf("cleared UpgradeCode is rewritten every run: %s", raw(patch))
	}
}

func TestMacDerivationAllowsExplicitReplacement(t *testing.T) {
	app := plugin.Subject{ID: "Payload/Example.app", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Name: "Example", MinimumOS: "14.1"}}
	req := macRequest(object{"included_apps": []any{object{"id": "org.example.app", "version": "explicit-version"}}}, &app, app)
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeObject(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if m["primaryBundleVersion"] != "explicit-version" || origins["primary_bundle_version"] != "included_apps" {
		t.Fatalf("missing facts defeated explicit fields: %+v / %+v", m, origins)
	}
	req.Metadata = raw(object{})
	if _, _, err := Derive(req); err == nil || !strings.Contains(err.Error(), "included_apps") {
		t.Fatalf("versionless application became detection: %v", err)
	}
}
