package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectExpressionsResolveNativeValuesBeforeDecoding(t *testing.T) {
	t.Setenv("STEMMA_TEST_VALUE", "secret: value\nother: true")
	t.Setenv("STEMMA_TEST_DATA", "{{ facts.unavailable }}")
	p, err := parseTest(t, []byte(projectFixture+`  plugins:
    fixture:
      trusted: '{{ true }}'
      path: plugins/fixture
  destinations:
    repo:
      operation: fixture.publish
      config:
        token: '{{ env.STEMMA_TEST_VALUE }}'
        data: '{{ env.STEMMA_TEST_DATA }}'
        enabled: '{{ true }}'
        retries: '{{ 3 }}'
        options: '{{ ["one", "two"] }}'
---
`+resourceFixture))
	if err != nil {
		t.Fatal(err)
	}
	settings := p.Destinations["repo"].Config
	if !p.Plugins["fixture"].Trusted || settings["token"] != "secret: value\nother: true" || settings["enabled"] != true || settings["retries"] != float64(3) || len(settings["options"].([]any)) != 2 {
		t.Fatalf("project expressions lost native types: %#v", settings)
	}
	if settings["data"] != "{{ facts.unavailable }}" {
		t.Fatal("evaluated environment data was interpreted as authored configuration")
	}
}

func TestLoadRetainsResourceExpressionsUntilTheirEvaluationPhase(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", projectFixture+`  components:
    base:
      destinations:
        repo:
          title: '{{ env.STEMMA_TEST_TITLE }} {{ facts.app.version }}'
          description: '\{{ literal }}'
  destinations:
    repo:
      operation: fixture.publish
      config:
        token: '{{ env.STEMMA_TEST_SECRET }}'
`)
	writeConfig(t, root, "app.software.yaml", resourceFixture+"  extends: base\n")
	t.Setenv("STEMMA_TEST_SECRET", "")
	if err := os.Unsetenv("STEMMA_TEST_SECRET"); err != nil {
		t.Fatal(err)
	}
	if found, err := FindRoot(root); err != nil || found != root {
		t.Fatalf("credential-free root discovery: %q, %v", found, err)
	}
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
		t.Fatal("loading accepted a missing connection environment variable")
	}
	t.Setenv("STEMMA_TEST_SECRET", "test-secret")
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := p.Resources["stemma/v1alpha1/MacSoftware/app"].Spec["destinations"].(map[string]any)["repo"].(map[string]any)
	if metadata["title"] != "{{ env.STEMMA_TEST_TITLE }} {{ facts.app.version }}" || metadata["description"] != `\{{ literal }}` || p.Destinations["repo"].Config["token"] != "test-secret" {
		t.Fatal("loading changed an authored resource expression or failed to resolve connection settings")
	}
}

func TestExpressionsRejectInvalidTypesAndDynamicIdentities(t *testing.T) {
	base := projectFixture + "---\n" + resourceFixture
	for name, document := range map[string]string{
		"dynamic key":             base + "    '{{ env.STEMMA_TEST_VALUE }}': value\n",
		"project identity":        strings.Replace(base, "name: catalog", "name: '{{ \"catalog\" }}'", 1),
		"resource identity":       strings.Replace(base, "name: app", "name: '{{ \"app\" }}'", 1),
		"import":                  strings.Replace(base, "'*.software.yaml'", "'{{ \"*.software.yaml\" }}'", 1),
		"inheritance":             base + "  extends: '{{ \"base\" }}'\n",
		"plugin boolean":          strings.Replace(base, "  imports:", "  plugins:\n    fixture:\n      trusted: '{{ \"false\" }}'\n      path: fixture\n  imports:", 1),
		"connection late context": strings.Replace(base, "  imports:", "  destinations:\n    repo:\n      operation: fixture.publish\n      config:\n        token: '{{ facts.app.version }}'\n  imports:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTest(t, []byte(document)); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestExpressionsPreserveShellInterpolation(t *testing.T) {
	script := "#!/bin/sh\n/bin/mkdir -p \"${config%/*}\"\necho \"${NAME:-default}\"\n"
	document := projectFixture + "---\n" + resourceFixture + "  destinations:\n    repo:\n      script: |\n        " + strings.ReplaceAll(strings.TrimSuffix(script, "\n"), "\n", "\n        ") + "\n"
	p, err := parseTest(t, []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	got := p.Resources["stemma/v1alpha1/MacSoftware/app"].Spec["destinations"].(map[string]any)["repo"].(map[string]any)["script"]
	if got != script {
		t.Fatalf("shell script changed: %q", got)
	}
}
