package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentValuesRemainStrings(t *testing.T) {
	for _, value := range []string{"secret: value\nother: true", "false", "123", "null", "[one, two]", "", "${NOT_EXPANDED_AGAIN}", "$literal"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("STEMMA_TEST_VALUE", value)
			p, err := parseTest(t, []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: test
spec:
  components:
    base:
      destinations:
        repo:
          description: ${STEMMA_TEST_VALUE}
          catalogs:
            - ${STEMMA_TEST_VALUE}
  destinations:
    repo:
      operation: fixture.publish
      config:
        token: ${STEMMA_TEST_VALUE}
  imports:
    - '*.software.yaml'
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: app
spec:
  extends: base
`))
			if err != nil {
				t.Fatal(err)
			}
			metadata := p.Resources["stemma/v1alpha1/MacSoftware/app"].Spec["destinations"].(map[string]any)["repo"].(map[string]any)
			if metadata["description"] != value || metadata["catalogs"].([]any)[0] != value || p.Destinations["repo"].Config["token"] != value {
				t.Fatal("environment value was changed, reinterpreted or not expanded")
			}
		})
	}
}

func TestEnvironmentRejectsMissingWholeValue(t *testing.T) {
	t.Setenv("STEMMA_TEST_MISSING", "")
	if err := os.Unsetenv("STEMMA_TEST_MISSING"); err != nil {
		t.Fatal(err)
	}
	document := projectFixture + "---\n" + resourceFixture + "    token: ${STEMMA_TEST_MISSING}\n"
	if _, err := parseTest(t, []byte(document)); err == nil || !strings.Contains(err.Error(), "environment variable STEMMA_TEST_MISSING is not set") {
		t.Fatalf("missing environment value: %v", err)
	}
}

func TestEnvironmentPreservesEmbeddedScriptExpressions(t *testing.T) {
	t.Setenv("STEMMA_TEST_VALUE", "runner-value")
	script := "#!/bin/sh\n/bin/mkdir -p \"${config%/*}\"\necho \"${STEMMA_TEST_VALUE} ${NAME:-default}\"\n"
	document := projectFixture + "---\n" + `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: policy
spec:
  destinations:
    external:
      postinstall_script: |
        ` + strings.ReplaceAll(strings.TrimSuffix(script, "\n"), "\n", "\n        ") + "\n"
	p, err := parseTest(t, []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	got := p.Resources["stemma/v1alpha1/MacSoftware/policy"].Spec["destinations"].(map[string]any)["external"].(map[string]any)["postinstall_script"]
	if got != script {
		t.Fatalf("native script changed: %q", got)
	}
}

func TestLoadExpandsImportedValuesWithoutRequiringEnvironmentForDiscovery(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", strings.Replace(projectFixture, "'*.software.yaml'", "software/stemma.yaml", 1)+`  destinations:
    external:
      operation: fixture.publish
      config:
        token: ${STEMMA_TEST_SECRET}
`)
	writeConfig(t, root, "software/stemma.yaml", resourceFixture+"    token: ${STEMMA_TEST_SECRET}\n")
	t.Setenv("STEMMA_TEST_SECRET", "")
	if err := os.Unsetenv("STEMMA_TEST_SECRET"); err != nil {
		t.Fatal(err)
	}
	if found, err := FindRoot(filepath.Join(root, "software")); err != nil || found != root {
		t.Fatalf("root discovery: %q, %v", found, err)
	}
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil || !strings.Contains(err.Error(), "STEMMA_TEST_SECRET") {
		t.Fatalf("missing environment: %v", err)
	}
	t.Setenv("STEMMA_TEST_SECRET", "test-secret")
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Destinations["external"].Config["token"] != "test-secret" || p.Resources["stemma/v1alpha1/MacSoftware/app"].Spec["source"].(map[string]any)["token"] != "test-secret" {
		t.Fatal("root or imported values were not expanded")
	}
	data, err := os.ReadFile(filepath.Join(root, "software/stemma.yaml"))
	if err != nil || !strings.Contains(string(data), "${STEMMA_TEST_SECRET}") {
		t.Fatal("loading rewrote the configuration file")
	}
}

func TestEnvironmentDoesNotExpandKeysOrBypassStrictTypes(t *testing.T) {
	t.Setenv("STEMMA_TEST_VALUE", "private-test-value")
	base := projectFixture + "---\n" + resourceFixture
	for _, document := range []string{
		strings.Replace(base, "  imports:", "  destinations:\n    repo:\n      operation: fixture.publish\n      config:\n        '${STEMMA_TEST_VALUE}': value\n  imports:", 1),
		base + "unknown: '${STEMMA_TEST_VALUE}'\n",
		strings.Replace(base, "  imports:", "  plugins:\n    fixture:\n      trusted: '${STEMMA_TEST_VALUE}'\n      image: ghcr.io/example/fixture:v1\n  imports:", 1),
	} {
		if _, err := parseTest(t, []byte(document)); err == nil || strings.Contains(err.Error(), "private-test-value") {
			t.Fatalf("invalid configuration was accepted or exposed an environment value: %v", err)
		}
	}
}

func TestResourceHeadersRemainLiteral(t *testing.T) {
	t.Setenv("STEMMA_TEST_NAME", "catalog")
	for _, replace := range []struct{ project, resource string }{
		{strings.Replace(projectFixture, "name: catalog", "name: '${STEMMA_TEST_NAME}'", 1), resourceFixture},
		{projectFixture, strings.Replace(resourceFixture, "name: app", "name: '${STEMMA_TEST_NAME}'", 1)},
	} {
		root := t.TempDir()
		writeConfig(t, root, "stemma.yaml", replace.project)
		writeConfig(t, root, "app.software.yaml", replace.resource)
		if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil || !strings.Contains(err.Error(), "metadata.name") {
			t.Fatalf("resource identity depends on runner environment: %v", err)
		}
	}
}
