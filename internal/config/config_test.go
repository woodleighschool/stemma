package config

import (
	"strings"
	"testing"
)

func TestPluginSources(t *testing.T) {
	for _, test := range []struct {
		plugin Plugin
		valid  bool
	}{
		{Plugin{Image: "ghcr.io/example/plugin:v1"}, true},
		{Plugin{Image: "ghcr.io/example/plugin@sha256:" + strings.Repeat("a", 64)}, true},
		{Plugin{Image: "ghcr.io/example/plugin"}, false},
		{Plugin{Image: "https://ghcr.io/example/plugin:v1"}, false},
		{Plugin{Path: "plugins/probe"}, true},
		{Plugin{Path: "/opt/plugins/probe"}, true},
		{Plugin{Path: "plugins/probe", Entrypoint: "bin/run"}, true},
		{Plugin{Path: "plugins/probe", Entrypoint: "../run"}, false},
		{Plugin{Path: "plugins/probe", Image: "ghcr.io/example/plugin:v1"}, false},
		{Plugin{}, false},
	} {
		resource := Resource{APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Metadata: Metadata{Name: "fixture"}, Spec: map[string]any{}}
		project := Project{Project: "test", Resources: map[string]Resource{resource.Reference().Key(): resource}, Plugins: map[string]Plugin{"fixture": test.plugin}}
		if err := project.Validate(); (err == nil) != test.valid {
			t.Fatalf("plugin=%+v valid=%v error=%v", test.plugin, test.valid, err)
		}
	}
}

func TestCompositionRetainsPresence(t *testing.T) {
	p, err := parseTest(t, []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: test
spec:
  imports:
    - '*.software.yaml'
  components:
    base:
      source:
        url: https://example.test/a.pkg
      destinations:
        repo:
          pkginfo:
            description: inherited
            unattended_install: true
            catalogs:
              - testing
  destinations:
    repo:
      operation: fixture.publish
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: app
spec:
  extends: base
  destinations:
    repo:
      pkginfo:
        description: null
        unattended_install: false
        catalogs: []
`))
	if err != nil {
		t.Fatal(err)
	}
	spec := p.Resources["stemma/v1alpha1/MacSoftware/app"].Spec
	metadata := spec["destinations"].(map[string]any)["repo"].(map[string]any)["pkginfo"].(map[string]any)
	if value, present := metadata["description"]; !present || value != nil || metadata["unattended_install"] != false || len(metadata["catalogs"].([]any)) != 0 {
		t.Fatalf("composition lost field presence: %#v", metadata)
	}
	metadata["description"] = "edited"
	original := p.Components["base"]["destinations"].(map[string]any)["repo"].(map[string]any)["pkginfo"].(map[string]any)
	if original["description"] != "inherited" {
		t.Fatal("composition mutated the shared component")
	}
}

func TestAliasesRepeatAnchoredValues(t *testing.T) {
	p, err := parseTest(t, []byte(projectFixture+"---\n"+resourceFixture+`  destinations:
    repo:
      publisher: &publisher Vendor
      developer: *publisher
      signing: &signing
        team: ABCDE12345
      verification: *signing
`))
	if err != nil {
		t.Fatal(err)
	}
	metadata := p.Resources["stemma/v1alpha1/MacSoftware/app"].Spec["destinations"].(map[string]any)["repo"].(map[string]any)
	if metadata["developer"] != "Vendor" || metadata["verification"].(map[string]any)["team"] != "ABCDE12345" {
		t.Fatalf("aliases did not repeat anchored values: %#v", metadata)
	}
	metadata["verification"].(map[string]any)["team"] = "edited"
	if metadata["signing"].(map[string]any)["team"] != "ABCDE12345" {
		t.Fatal("an alias shares its anchored value")
	}
}

func TestRejectMalformedConfiguration(t *testing.T) {
	base := projectFixture + "---\n" + resourceFixture
	for name, document := range map[string]string{
		"unknown-envelope":     base + "typo: true\n",
		"duplicate-field":      base + "kind: MacSoftware\n",
		"malformed-document":   base + "---\nversion: 1\n",
		"nonfinite":            strings.Replace(base, "  imports:", "  destinations:\n    fixture:\n      operation: fixture.publish\n      config:\n        value: .nan\n  imports:", 1),
		"old-connection-shape": strings.Replace(base, "  imports:", "  destinations:\n    fixture:\n      type: fixture.publish\n  imports:", 1),
		"cycle":                strings.Replace(strings.Replace(base, "  imports:", "  components:\n    a:\n      extends: b\n    b:\n      extends: a\n  imports:", 1), "  source:", "  extends: a\n  source:", 1),
		"merge-key":            strings.Replace(base, "  source:", "  <<: {}\n  source:", 1),
		"cross-document-alias": strings.Replace(strings.Replace(base, "name: catalog", "name: &name catalog", 1), "name: app", "name: *name", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTest(t, []byte(document)); err == nil {
				t.Fatal("accepted malformed configuration")
			}
		})
	}
}

func TestDestinationOperationNames(t *testing.T) {
	for _, operation := range []string{"fixture/publish", "fixture.publish", "fixture_publish", "fixture-publish"} {
		if err := validateOperation(operation); err != nil {
			t.Fatalf("valid external operation %q: %v", operation, err)
		}
	}
	for _, operation := range []string{"", "Fixture.publish", "fixture/../publish", "fixture publish"} {
		if err := validateOperation(operation); err == nil {
			t.Fatalf("accepted invalid operation %q", operation)
		}
	}
}
