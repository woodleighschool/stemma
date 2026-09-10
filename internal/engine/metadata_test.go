package engine

import (
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestMetadataFactReferencesPreserveTypesAndOverrides(t *testing.T) {
	facts := metadataFacts()
	software := config.Software{Subjects: map[string]config.SubjectSelector{
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
	effective, origins, err := resolveMetadata(software, native, facts)
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
			software := config.Software{Subjects: map[string]config.SubjectSelector{"app": {Kind: "app"}}, Destinations: map[string]map[string]any{"repo": {"version": reference}}}
			if err := validateReferences(software); err == nil {
				t.Fatal("invalid reference passed static validation")
			}
		})
	}
	software := config.Software{Subjects: map[string]config.SubjectSelector{"app": {Kind: "app"}}}
	for _, reference := range []string{"app.app.executable", "app.msi.product_code"} {
		if _, _, err := resolveMetadata(software, map[string]any{"version": factReference(reference)}, metadataFacts()); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("absent observed value did not fail: %v", err)
		}
	}
	facts := metadataFacts()
	facts.Subjects = append(facts.Subjects, plugin.Subject{ID: "helper", Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.helper", Version: "9"}})
	native := map[string]any{"version": factReference("app.app.version")}
	if _, _, err := resolveMetadata(software, native, facts); err == nil || !strings.Contains(err.Error(), "matched 2") {
		t.Fatalf("ambiguous subject selected the first match: %v", err)
	}
	if _, _, err := resolveMetadata(software, native, plugin.Facts{}); err == nil || !strings.Contains(err.Error(), "matched 0") {
		t.Fatalf("absent subject did not fail: %v", err)
	}
}

func TestStepConfigurationReferencesAreValidatedBeforeAcquisition(t *testing.T) {
	software := config.Software{
		Subjects: map[string]config.SubjectSelector{"app": {Kind: "app"}},
		Steps:    []config.Step{{Name: "render", Operation: "munki.pkginfo", Config: map[string]any{"version": factReference("app.app.build")}}},
	}
	if err := validateReferences(software); err != nil {
		t.Fatal(err)
	}
	software.Steps[0].Config["version"] = factReference("app.app.nonexistent")
	if err := validateReferences(software); err == nil || !strings.Contains(err.Error(), "step render") {
		t.Fatalf("invalid step fact was not rejected before execution: %v", err)
	}
}

func metadataFacts() plugin.Facts {
	return plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{
		{ID: "payload.pkg", Kind: "package", Path: "payload.pkg", Package: &plugin.PackageFacts{Identifier: "org.example.pkg", Version: "1.2", HasPayload: true}},
		{ID: "app", Parent: "payload.pkg", Kind: "app", Path: "payload.pkg/Applications/App.app", InstalledPath: "/Applications/App.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "1.2", Build: "42", MinimumOS: "13.0"}},
	}}
}

func factReference(name string) map[string]any { return map[string]any{"$fact": name} }
