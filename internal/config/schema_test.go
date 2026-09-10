package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
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
	for name, fields := range map[string][]string{"Metadata": {"name"}, "ProjectSpec": {"imports", "components"}, "Source": {"type", "url", "sha256", "release"}, "Destination": {"operation", "config"}, "Step": {"operation", "inputs"}, "SubjectSelector": {"kind", "installed_path", "bundle_id"}, "Verification": {"subject", "integrity"}, "MunkiMetadata": {"description", "catalogs", "unattended_install"}, "IntuneConnection": {"token", "client_id"}, "JamfMetadata": {"package_id", "categoryId"}} {
		for _, field := range fields {
			if schema.Definitions[name].Properties[field].Description == "" {
				t.Errorf("%s.%s lacks editor hover description", name, field)
			}
		}
	}
}

func TestSoftwareDocumentSchemaAndLoaderAgree(t *testing.T) {
	project := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: catalog}
spec: {imports: [software.yaml]}
`
	software := `apiVersion: stemma/v1alpha1
kind: Software
metadata: {name: app}
spec: {source: {type: file, path: vendor.pkg}}
`
	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	projectWithDestination := strings.Replace(project, "imports: [software.yaml]", "imports: [software.yaml], destinations: {repo: {operation: munki, path: repo}}", 1)
	projectWithSource := strings.Replace(project, "imports: [software.yaml]", "imports: [software.yaml], components: {base: {source: {type: file, path: vendor.pkg}}}", 1)
	for name, test := range map[string]struct {
		project, software string
		valid             bool
	}{
		"documents":                   {project, software, true},
		"source-free":                 {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "", 1), true},
		"null-source":                 {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "source: null", 1), true},
		"inherited-source":            {projectWithSource, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "extends: base, select: App.app", 1), true},
		"removed-source":              {projectWithSource, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "extends: base, source: null", 1), true},
		"empty-source":                {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "source: {}", 1), false},
		"select-no-source":            {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "select: App.app", 1), false},
		"empty-inheritance":           {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "extends: '', select: App.app", 1), false},
		"artifact-no-source":          {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "artifacts: {package: {type: pkg, identifier: org.example.fixture, version: '1'}}", 1), false},
		"step-no-source":              {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "steps: [{name: inspect, operation: inspect, inputs: {input: source}}]", 1), false},
		"destination-no-source":       {projectWithDestination, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "destinations: {repo: {artifact: prepared}}", 1), false},
		"destination-input-no-source": {projectWithDestination, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "destinations: {repo: {inputs: {installer: source}}}", 1), false},
		"verify-no-source":            {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "verification: {subject: prepared}", 1), false},
		"render-no-source":            {project, strings.Replace(software, "source: {type: file, path: vendor.pkg}", "steps: [{name: render, operation: munki.pkginfo, config: {name: fixture, version: '1', installer_type: nopkg}}]", 1), true},
		"old-root":                    {"version: 1\nproject: catalog\nrecipes: {}\n", software, false},
		"inline-items":                {project + "recipes: {}\n", software, false},
		"wrong-version":               {project, strings.Replace(software, "v1alpha1", "v9", 1), false},
		"wrong-kind":                  {project, strings.Replace(software, "kind: Software", "kind: Recipe", 1), false},
		"missing-name":                {project, strings.Replace(software, "name: app", "", 1), false},
		"environment-name":            {project, strings.Replace(software, "name: app", "name: '${APP_NAME}'", 1), false},
		"invalid-name":                {project, strings.Replace(software, "name: app", "name: ../app", 1), false},
		"unknown-field":               {project, software + "unknown: true\n", false},
		"project-as-item":             {project, project, false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, "stemma.yaml", test.project)
			writeConfig(t, root, "software.yaml", test.software)
			_, err := Load(filepath.Join(root, "stemma.yaml"))
			if (err == nil) != test.valid {
				t.Fatalf("loader validity=%v: %v", test.valid, err)
			}
			// The document union validates shape; the loader also enforces which
			// kind belongs at a root or an import boundary.
			if name == "project-as-item" {
				return
			}
			valid := true
			for _, document := range []string{test.project, test.software} {
				var value any
				if err := yaml.Unmarshal([]byte(document), &value); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				valid = plugin.ValidateSchema(schema, encoded) == nil && valid
			}
			if valid != test.valid {
				t.Fatalf("editor schema validity=%v, expected %v", valid, test.valid)
			}
		})
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
