package config

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestSchemaIncludesEditorDescriptions(t *testing.T) {
	data, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions map[string]struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	for name, fields := range map[string][]string{"Project": {"project", "recipes"}, "Source": {"type", "url", "sha256", "release"}, "Destination": {"operation", "config"}, "Step": {"operation", "inputs"}, "SubjectSelector": {"kind", "installed_path", "bundle_id"}, "Verification": {"subject", "integrity"}, "MunkiMetadata": {"description", "catalogs", "unattended_install"}, "IntuneConnection": {"token", "client_id"}, "JamfMetadata": {"package_id", "categoryId"}} {
		for _, field := range fields {
			if schema.Definitions[name].Properties[field].Description == "" {
				t.Errorf("%s.%s lacks editor hover description", name, field)
			}
		}
	}
}

func TestNativeSchemaAcceptsTypedReferencesAndRejectsUnknownFields(t *testing.T) {
	data, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	delete(schema, "oneOf")
	for name, test := range map[string]struct {
		definition string
		data       string
		valid      bool
	}{
		"munki-values":          {"MunkiMetadata", `{"version":{"$fact":"app.app.version"},"unattended_install":{"$fact":"pkg.package.has_payload"},"minimum_os_version":{"$fact":"app.app.minimum_os"}}`, true},
		"munki-presence":        {"MunkiMetadata", `{"description":null,"unattended_install":false,"installs":[]}`, true},
		"additional-inputs":     {"MunkiMetadata", `{"artifact":"pkginfo/artifact","inputs":{"installer":"contents/artifact"}}`, true},
		"invalid-input-type":    {"MunkiMetadata", `{"inputs":{"installer":{"$fact":"app.path"}}}`, false},
		"intune-apps":           {"IntuneMetadata", `{"@odata.type":"#microsoft.graph.macOSPkgApp","includedApps":[{"bundleId":{"$fact":"app.app.bundle_id"},"bundleVersion":{"$fact":"app.app.version"}}],"ignoreVersionDetection":{"$fact":"pkg.package.has_payload"},"minimumSupportedOperatingSystem":{"v13_0":{"$fact":"pkg.package.has_payload"}}}`, true},
		"intune-derived-type":   {"IntuneMetadata", `{"includedApps":{"$fact":"app.msi.properties"},"minimumSupportedOperatingSystem":{"$fact":"app.msi.properties"}}`, true},
		"intune-type-reference": {"IntuneMetadata", `{"@odata.type":{"$fact":"app.kind"},"includedApps":[]}`, false},
		"unknown-native-field":  {"MunkiMetadata", `{"unknown":{"$fact":"app.app.version"}}`, false},
		"unknown-nested-field":  {"IntuneMetadata", `{"@odata.type":"#microsoft.graph.macOSPkgApp","includedApps":[{"bundleId":"org.example.app","bundleVersion":"1","typo":true}]}`, false},
		"mixed-reference":       {"MunkiMetadata", `{"version":{"$fact":"app.app.version","fallback":"1"}}`, false},
		"invalid-reference":     {"MunkiMetadata", `{"version":{"$fact":"app..version"}}`, false},
		"literal-boolean-type":  {"MunkiMetadata", `{"unattended_install":"false"}`, false},
		"verification-output":   {"Verification", `{"subject":"build/artifact","integrity":true}`, true},
		"verification-payload":  {"Verification", `{"subject":"payload","integrity":true}`, true},
		"verification-bad-path": {"Verification", `{"subject":"build/artifact/child"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			schema["$ref"] = "#/$defs/" + test.definition
			encoded, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			if err := plugin.ValidateSchema(encoded, []byte(test.data)); (err == nil) != test.valid {
				t.Fatalf("schema validity=%t, expected %t: %v", err == nil, test.valid, err)
			}
		})
	}
}
