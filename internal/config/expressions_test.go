package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectionExpressionsResolveNativeValuesWhenUsed(t *testing.T) {
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
	if !p.Plugins["fixture"].Trusted {
		t.Fatal("plugin declaration did not resolve while loading")
	}
	if p.Destinations["repo"].Config["token"] != "{{ env.STEMMA_TEST_VALUE }}" {
		t.Fatal("loading evaluated connection settings")
	}
	settings, err := p.Destinations["repo"].ResolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"data":"{{ facts.unavailable }}","enabled":true,"options":["one","two"],"retries":3,"token":"secret: value\nother: true"}`; string(got) != want {
		t.Fatalf("resolved settings lost native types or evaluated environment data again:\n got %s\nwant %s", got, want)
	}
}

func TestLoadKeepsConnectionExpressionsUntilUse(t *testing.T) {
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
  reconcile:
    source_control:
      type: github
      config:
        private_key: '{{ env.STEMMA_TEST_KEY }}'
`)
	writeConfig(t, root, "app.software.yaml", resourceFixture+"  extends: base\n")
	for _, name := range []string{"STEMMA_TEST_SECRET", "STEMMA_TEST_KEY"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	if found, err := FindRoot(root); err != nil || found != root {
		t.Fatalf("credential-free root discovery: %q, %v", found, err)
	}
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatalf("loading required connection environment values: %v", err)
	}
	metadata := p.Resources["stemma/v1alpha1/MacSoftware/app"].Spec["destinations"].(map[string]any)["repo"].(map[string]any)
	if metadata["title"] != "{{ env.STEMMA_TEST_TITLE }} {{ facts.app.version }}" || metadata["description"] != `\{{ literal }}` || p.Destinations["repo"].Config["token"] != "{{ env.STEMMA_TEST_SECRET }}" || p.Reconcile.SourceControl.Config["private_key"] != "{{ env.STEMMA_TEST_KEY }}" {
		t.Fatal("loading changed an expression")
	}
	if _, err := p.Destinations["repo"].ResolvedConfig(); err == nil || !strings.Contains(err.Error(), "required reference is missing") {
		t.Fatalf("connecting accepted a missing environment value: %v", err)
	}
	if _, err := p.Reconcile.SourceControl.ResolvedConfig(); err == nil {
		t.Fatal("source control accepted a missing environment value")
	}
	if _, err := p.Resolved(); err == nil {
		t.Fatal("resolved project accepted a missing environment value")
	}
	t.Setenv("STEMMA_TEST_SECRET", "test-secret")
	t.Setenv("STEMMA_TEST_KEY", "test-key")
	resolved, err := p.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Destinations["repo"].Config["token"] != "test-secret" || resolved.Reconcile.SourceControl.Config["private_key"] != "test-key" || p.Destinations["repo"].Config["token"] != "{{ env.STEMMA_TEST_SECRET }}" {
		t.Fatal("resolving did not evaluate connection settings or changed the loaded project")
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
