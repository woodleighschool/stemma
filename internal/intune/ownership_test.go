package intune

import (
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestDerivedFieldWithoutArtifactValueIsClearedOrMustBeSet(t *testing.T) {
	// An MSI need not declare an UpgradeCode or a manufacturer, and an application
	// need not name itself. Graph clears the first with null and requires the rest.
	msi := plugin.Subject{Kind: "msi", MSI: &plugin.MSIFacts{ProductName: "Example", ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0"}}
	app := plugin.Subject{Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "2.0", MinimumOS: "14.0"}}
	installer := plugin.Artifact{Filename: "Example.msi", Facts: plugin.Facts{Subjects: []plugin.Subject{msi}}}
	for _, test := range []struct {
		name     string
		metadata object
		subject  plugin.Subject
		artifact plugin.Artifact
		missing  string
		check    func(object) bool
	}{
		{name: "selected MSI", metadata: object{"derive": object{"msi": "main"}}, subject: msi, missing: "publisher"},
		{name: "selected MSI declared", metadata: object{"derive": object{"msi": "main"}, "publisher": "Vendor", "msiInformation": object{"publisher": "Vendor"}}, subject: msi, check: func(m object) bool {
			info := m["msiInformation"].(object)
			cleared, owned := info["upgradeCode"]
			return owned && cleared == nil && info["publisher"] == "Vendor" && info["productVersion"] == "2.0"
		}},
		{name: "setup MSI", metadata: object{"type": "win32"}, artifact: installer, missing: "msiInformation.publisher"},
		{name: "application", metadata: object{"type": "pkg", "derive": object{"app": "main"}}, subject: app, missing: "displayName"},
		{name: "application declared", metadata: object{"type": "pkg", "derive": object{"app": "main"}, "displayName": "Example"}, subject: app, check: func(m object) bool {
			return m["displayName"] == "Example" && m["primaryBundleId"] == "org.example.app"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := plugin.ReconcileRequest[Config]{
				Prepared: true, Config: Config{GraphURL: "https://graph.microsoft.com/v1.0", Token: "synthetic"}, Metadata: raw(test.metadata), Artifact: test.artifact,
				Subjects: map[string]plugin.SubjectSelector{"main": {Kind: test.subject.Kind}},
				Facts:    plugin.Facts{Subjects: []plugin.Subject{test.subject}},
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
				if m, err := validateMetadata(derived.Metadata); err != nil || !test.check(m) {
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
	// Intune has no setting for macOS 14.1, so the document settles the requirement.
	req := plugin.ReconcileRequest[Config]{
		Method: "validate", Prepared: true,
		Subjects: map[string]plugin.SubjectSelector{"main": {Kind: "app"}},
		Facts:    plugin.Facts{Subjects: []plugin.Subject{{Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.app", Name: "Example", MinimumOS: "14.1"}}}},
		Metadata: raw(object{"type": "pkg", "derive": object{"app": "main"}, "primaryBundleVersion": "explicit-version", "minimumSupportedOperatingSystem": object{"v14_0": true}}),
	}
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	minimum, _ := m["minimumSupportedOperatingSystem"].(object)
	if m["primaryBundleVersion"] != "explicit-version" || origins["primaryBundleVersion"] != "" || minimum["v14_0"] != true || origins["minimumSupportedOperatingSystem"] != "" {
		t.Fatalf("missing or unrepresentable facts defeated explicit fields: %+v / %+v", m, origins)
	}
}
