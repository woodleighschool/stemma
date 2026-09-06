package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestMetadataFactReferencesPreserveTypesAndOverrides(t *testing.T) {
	facts := metadataFacts()
	recipe := config.Recipe{Subjects: map[string]config.SubjectSelector{
		"app": {BundleID: "org.example.app"},
		"pkg": {Kind: "package"},
	}}
	facts.Subjects[0].Package.HasPayload = false
	native := map[string]any{
		"version":            factReference("app.app.build"),
		"unattended_install": factReference("pkg.package.has_payload"),
		"installed_size":     factReference("pkg.package.installed_size"),
		"description":        nil,
		"receipts":           []any{},
		"installs":           []any{},
		"postinstall_script": "echo '{$fact: app.app.version}'",
	}
	before := config.Fingerprint(facts)
	effective, origins, err := resolveMetadata(recipe, native, facts, "munki")
	if err != nil {
		t.Fatal(err)
	}
	if effective["version"] != "42" || effective["unattended_install"] != false || effective["installed_size"] != float64(0) || effective["description"] != nil {
		t.Fatalf("typed reference values changed: %#v", effective)
	}
	if len(effective["receipts"].([]any)) != 0 || len(effective["installs"].([]any)) != 0 || effective["postinstall_script"] != native["postinstall_script"] {
		t.Fatal("explicit lists or literal script were changed")
	}
	if origins["version"] != "fact" || origins["description"] != "explicit" || config.Fingerprint(facts) != before {
		t.Fatal("origins or immutable observed facts changed")
	}
}

func TestMetadataReferencesRejectUnknownMissingAndAmbiguousFacts(t *testing.T) {
	for name, reference := range map[string]any{
		"unknown-subject": factReference("missing.app.version"),
		"unknown-field":   factReference("app.app.short_version"),
		"missing-field":   factReference("app."),
		"extra-segment":   factReference("app.app.version.value"),
		"non-string":      map[string]any{"$fact": 42},
		"mixed-object":    map[string]any{"$fact": "app.app.version", "fallback": "1"},
	} {
		t.Run(name, func(t *testing.T) {
			recipe := config.Recipe{Subjects: map[string]config.SubjectSelector{"app": {Kind: "app"}}, Destinations: map[string]map[string]any{"repo": {"version": reference}}}
			if err := validateReferences(recipe); err == nil {
				t.Fatal("invalid reference passed static validation")
			}
		})
	}
	recipe := config.Recipe{Subjects: map[string]config.SubjectSelector{"app": {Kind: "app"}}}
	for _, reference := range []string{"app.app.executable", "app.msi.product_code"} {
		if _, _, err := resolveMetadata(recipe, map[string]any{"version": factReference(reference)}, metadataFacts(), "munki"); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("absent observed value did not fail: %v", err)
		}
	}
	facts := metadataFacts()
	facts.Subjects = append(facts.Subjects, plugin.Subject{ID: "helper", Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.helper", Version: "9"}})
	native := map[string]any{"version": factReference("app.app.version")}
	if _, _, err := resolveMetadata(recipe, native, facts, "munki"); err == nil || !strings.Contains(err.Error(), "matched 2") {
		t.Fatalf("ambiguous subject selected the first match: %v", err)
	}
	if _, _, err := resolveMetadata(recipe, native, plugin.Facts{}, "munki"); err == nil || !strings.Contains(err.Error(), "matched 0") {
		t.Fatalf("absent subject did not fail: %v", err)
	}
}

func TestStepConfigurationReferencesAreValidatedBeforeAcquisition(t *testing.T) {
	recipe := config.Recipe{
		Subjects: map[string]config.SubjectSelector{"app": {Kind: "app"}},
		Steps:    []config.Step{{Name: "render", Operation: "munki.pkginfo", Config: map[string]any{"version": factReference("app.app.build")}}},
	}
	if err := validateReferences(recipe); err != nil {
		t.Fatal(err)
	}
	recipe.Steps[0].Config["version"] = factReference("app.app.nonexistent")
	if err := validateReferences(recipe); err == nil || !strings.Contains(err.Error(), "step render") {
		t.Fatalf("invalid step fact was not rejected before execution: %v", err)
	}
}

func TestMunkiDefaultsRespectDetectionPolicyAndVersionIdentity(t *testing.T) {
	facts := metadataFacts()
	facts.Subjects = append(facts.Subjects, plugin.Subject{ID: "scripts", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.scripts", Version: "1.2"}})
	native, _, err := resolveMetadata(config.Recipe{}, nil, facts, "munki")
	if err != nil {
		t.Fatal(err)
	}
	if native["version"] != "1.2" || len(native["receipts"].([]any)) != 1 || len(native["installs"].([]any)) != 1 {
		t.Fatalf("wrong native package defaults: %#v", native)
	}
	for _, policy := range []map[string]any{{"installcheck_script": "exit 1"}, {"receipts": []any{}}, {"installs": []any{}}} {
		effective, _, err := resolveMetadata(config.Recipe{}, policy, facts, "munki")
		if err != nil {
			t.Fatal(err)
		}
		if installs, exists := effective["installs"]; exists && len(installs.([]any)) != 0 {
			t.Fatalf("explicit detection policy gained an application condition: %#v", effective)
		}
	}
	facts.Subjects[1].App.Version = "2.0"
	native, _, err = resolveMetadata(config.Recipe{}, nil, facts, "munki")
	if err != nil {
		t.Fatal(err)
	}
	if _, guessed := native["version"]; guessed {
		t.Fatal("different observed package and app versions were collapsed")
	}
}

func TestIntuneDefaultsUseOnlyIntendedApps(t *testing.T) {
	facts := metadataFacts()
	effective, origins, err := resolveMetadata(config.Recipe{}, nil, facts, "intune")
	if err != nil {
		t.Fatal(err)
	}
	if effective["@odata.type"] != "#microsoft.graph.macOSPkgApp" || effective["primaryBundleId"] != "org.example.app" || effective["primaryBundleVersion"] != "1.2" || len(effective["includedApps"].([]any)) != 1 || !reflect.DeepEqual(effective["minimumSupportedOperatingSystem"], map[string]any{"v13_0": true}) || origins["includedApps"] != "derived" {
		t.Fatalf("incomplete Intune defaults: %#v", effective)
	}
	facts.Subjects = append(facts.Subjects, plugin.Subject{ID: "helper", Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.helper", Version: "99", MinimumOS: "26.4"}})
	native := map[string]any{
		"includedApps":           []any{map[string]any{"bundleId": "org.example.app", "bundleVersion": "1.2"}},
		"ignoreVersionDetection": false,
	}
	effective, _, err = resolveMetadata(config.Recipe{}, native, facts, "intune")
	if err != nil {
		t.Fatal(err)
	}
	if effective["primaryBundleId"] != "org.example.app" || len(effective["includedApps"].([]any)) != 1 || effective["ignoreVersionDetection"] != false || !reflect.DeepEqual(effective["minimumSupportedOperatingSystem"], map[string]any{"v13_0": true}) {
		t.Fatalf("helper app altered explicit detection: %#v", effective)
	}
	native["primaryBundleId"] = "org.example.helper"
	if _, _, err := resolveMetadata(config.Recipe{}, native, facts, "intune"); err == nil {
		t.Fatal("primary bundle diverged from the intended first app")
	}
	for _, explicit := range []any{nil, []any{}} {
		effective, _, err := resolveMetadata(config.Recipe{}, map[string]any{"includedApps": explicit}, facts, "intune")
		if err != nil || !reflect.DeepEqual(effective["includedApps"], explicit) {
			t.Fatalf("invalid explicit list was repaired before native validation: %#v %v", effective, err)
		}
	}
	if _, _, err = resolveMetadata(config.Recipe{}, nil, facts, "intune"); err == nil {
		t.Fatal("multiple apps did not require explicit detection selection")
	}
}

func TestIntuneMinimumOSNeverRoundsDown(t *testing.T) {
	for _, version := range []string{"10.15", "11", "13.0.0", "26.0"} {
		if _, err := intuneMinimumOS(version); err != nil {
			t.Fatalf("supported exact version %s: %v", version, err)
		}
	}
	for _, version := range []string{"10.15.7", "13.1", "26.4", "99.0", "13..0", "garbage"} {
		if _, err := intuneMinimumOS(version); err == nil {
			t.Fatalf("unsupported version %s was rounded to a weaker policy", version)
		}
	}
	facts := metadataFacts()
	facts.Subjects[1].App.MinimumOS = "13.3"
	if _, _, err := resolveMetadata(config.Recipe{}, nil, facts, "intune"); err == nil {
		t.Fatal("unsupported observed minimum was silently weakened")
	}
	if _, _, err := resolveMetadata(config.Recipe{}, map[string]any{"minimumSupportedOperatingSystem": map[string]any{"v14_0": true}}, facts, "intune"); err != nil {
		t.Fatalf("explicit supported eligibility policy was ignored: %v", err)
	}
	facts.Subjects = append(facts.Subjects, plugin.Subject{ID: "second", Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.second", Version: "3", MinimumOS: "14.0"}})
	included := []any{map[string]any{"bundleId": "org.example.app", "bundleVersion": "1.2"}, map[string]any{"bundleId": "org.example.second", "bundleVersion": "3"}}
	effective, _, err := resolveMetadata(config.Recipe{}, map[string]any{"includedApps": included}, facts, "intune")
	if err != nil || !reflect.DeepEqual(effective["minimumSupportedOperatingSystem"], map[string]any{"v14_0": true}) {
		t.Fatalf("strongest representable minimum did not cover all included apps: %#v %v", effective, err)
	}
}

func TestIntuneExplicitPrimaryPairOwnsDetectionSelection(t *testing.T) {
	facts := metadataFacts()
	facts.Subjects = append(facts.Subjects, plugin.Subject{ID: "helper", Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.helper", Version: "99"}})
	for name, native := range map[string]map[string]any{
		"pair": {"primaryBundleId": "org.example.app", "primaryBundleVersion": "7"},
		"id":   {"primaryBundleId": "org.example.app"},
	} {
		t.Run(name, func(t *testing.T) {
			effective, _, err := resolveMetadata(config.Recipe{}, native, facts, "intune")
			if err != nil {
				t.Fatal(err)
			}
			version := "1.2"
			if name == "pair" {
				version = "7"
			}
			if effective["primaryBundleId"] != "org.example.app" || effective["primaryBundleVersion"] != version || !reflect.DeepEqual(effective["includedApps"], []any{map[string]any{"bundleId": "org.example.app", "bundleVersion": version}}) {
				t.Fatalf("explicit primary fields did not own the derived detection list: %#v", effective)
			}
		})
	}
	effective, _, err := resolveMetadata(config.Recipe{}, map[string]any{"primaryBundleVersion": "7"}, metadataFacts(), "intune")
	if err != nil || effective["primaryBundleId"] != "org.example.app" || effective["primaryBundleVersion"] != "7" {
		t.Fatalf("explicit primary version was overwritten by app facts: %#v %v", effective, err)
	}
	authored := map[string]any{"@odata.type": "#microsoft.graph.macOSDmgApp", "primaryBundleId": "org.example.app", "primaryBundleVersion": "7", "minimumSupportedOperatingSystem": map[string]any{"v13_0": true}}
	if needsInspection(authored, "intune") {
		t.Fatal("an authored primary pair and minimum OS required payload inspection")
	}
	effective, _, err = resolveMetadata(config.Recipe{}, authored, plugin.Facts{}, "intune")
	if err != nil || effective["primaryBundleId"] != "org.example.app" || len(effective["includedApps"].([]any)) != 1 {
		t.Fatalf("complete authored detection did not resolve without app facts: %#v %v", effective, err)
	}
}

func TestInspectionIsRequiredOnlyForRequestedFacts(t *testing.T) {
	authored := map[string]any{"@odata.type": "#microsoft.graph.macOSDmgApp", "includedApps": []any{}, "minimumSupportedOperatingSystem": map[string]any{"v13_0": true}}
	if needsInspection(authored, "intune") || needsInspection(nil, "munki") || needsInspection(map[string]any{"@odata.type": "#microsoft.graph.win32LobApp"}, "intune") {
		t.Fatal("fully authored native policy unexpectedly requires payload inspection")
	}
	if !needsInspection(nil, "intune") || !needsInspection(map[string]any{"version": factReference("app.app.version")}, "munki") {
		t.Fatal("required observed facts did not request inspection")
	}
	recipe := config.Recipe{Subjects: map[string]config.SubjectSelector{"unused": {BundleID: "org.example.absent"}}}
	if needsInspection(authored, "intune") {
		t.Fatal("an unused subject declaration requested inspection")
	}
	if _, _, err := resolveMetadata(recipe, map[string]any{"version": "1"}, plugin.Facts{}, "munki"); err != nil {
		t.Fatalf("an unused subject declaration selected against unrelated delivery facts: %v", err)
	}
}

func metadataFacts() plugin.Facts {
	return plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{
		{ID: "payload.pkg", Kind: "package", Path: "payload.pkg", Package: &plugin.PackageFacts{Identifier: "org.example.pkg", Version: "1.2", HasPayload: true}},
		{ID: "app", Parent: "payload.pkg", Kind: "app", Path: "payload.pkg/Applications/App.app", InstalledPath: "/Applications/App.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "1.2", Build: "42", MinimumOS: "13.0"}},
	}}
}

func factReference(name string) map[string]any { return map[string]any{"$fact": name} }
