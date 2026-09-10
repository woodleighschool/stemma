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
      source: {type: http, url: 'https://example.test/app.pkg'}
      destinations:
        repo:
          description: ${STEMMA_TEST_VALUE}
          catalogs: ["${STEMMA_TEST_VALUE}"]
  destinations:
    repo: {operation: munki, config: {path: repo}}
    jamf:
      operation: jamf
      config:
        client_secret: ${STEMMA_TEST_VALUE}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: app
spec: {extends: base}
`))
			if err != nil {
				t.Fatal(err)
			}
			metadata := p.Software["app"].Destinations["repo"]
			if metadata["description"] != value || metadata["catalogs"].([]any)[0] != value || p.Destinations["jamf"].Config["client_secret"] != value {
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
	document := "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata: {name: test}\nspec: {imports: ['*.software.yaml']}\n---\napiVersion: stemma/v1alpha1\nkind: Software\nmetadata: {name: app}\nspec:\n  source:\n    type: http\n    url: https://example.test/app.pkg\n    token: '${STEMMA_TEST_MISSING}'\n"
	if _, err := parseTest(t, []byte(document)); err == nil || !strings.Contains(err.Error(), "environment variable STEMMA_TEST_MISSING is not set") {
		t.Fatalf("missing environment value: %v", err)
	}
}

func TestEnvironmentPreservesEmbeddedScriptExpressions(t *testing.T) {
	t.Setenv("STEMMA_TEST_VALUE", "runner-value")
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata: {name: test}\nspec: {imports: [software.yaml]}\n")
	script := "#!/bin/sh\n/bin/mkdir -p \"${config%/*}\"\necho \"${STEMMA_TEST_VALUE} ${NAME:-default}\"\n"
	writeConfig(t, root, "software.yaml", "apiVersion: stemma/v1alpha1\nkind: Software\nmetadata: {name: policy}\nspec:\n  steps:\n    - name: policy\n      operation: munki.pkginfo\n      config:\n        postinstall_script: |\n          "+strings.ReplaceAll(strings.TrimSuffix(script, "\n"), "\n", "\n          ")+"\n")
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Software["policy"].Steps[0].Config["postinstall_script"]; got != script {
		t.Fatalf("native script changed: %q", got)
	}
}

func TestLoadExpandsImportedValuesWithoutRequiringEnvironmentForDiscovery(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: test
spec:
  imports: [software/stemma.yaml]
  destinations:
    jamf:
      operation: jamf
      config:
        client_secret: ${STEMMA_TEST_SECRET}
`)
	writeConfig(t, root, "software/stemma.yaml", `apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: app
spec:
  source:
    type: http
    url: https://example.test/app.pkg
    token: ${STEMMA_TEST_SECRET}
`)
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
	if p.Destinations["jamf"].Config["client_secret"] != "test-secret" || p.Software["app"].Source.Token != "test-secret" {
		t.Fatal("root or imported values were not expanded")
	}
	data, err := os.ReadFile(filepath.Join(root, "software/stemma.yaml"))
	if err != nil || !strings.Contains(string(data), "${STEMMA_TEST_SECRET}") {
		t.Fatal("loading rewrote the authored configuration")
	}
}

func TestEnvironmentDoesNotExpandKeysOrBypassStrictTypes(t *testing.T) {
	t.Setenv("STEMMA_TEST_VALUE", "private-test-value")
	base := "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: test\nspec:\n  imports: ['*.software.yaml']\n---\napiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: app\nspec: {source: {type: http, url: 'https://example.test/app.pkg'}}\n"
	for _, document := range []string{
		strings.Replace(base, "  imports:", "  destinations: {repo: {operation: jamf, config: {'${STEMMA_TEST_VALUE}': value}}}\n  imports:", 1),
		base + "unknown: '${STEMMA_TEST_VALUE}'\n",
		strings.Replace(base, "type: http", "type: http, include: '${STEMMA_TEST_VALUE}'", 1),
		strings.Replace(base, "type: http", "type: http, token_env: OLD", 1),
	} {
		if _, err := parseTest(t, []byte(document)); err == nil || strings.Contains(err.Error(), "private-test-value") {
			t.Fatalf("invalid configuration was accepted or exposed an environment value: %v", err)
		}
	}
}

func TestResourceHeadersRemainLiteral(t *testing.T) {
	t.Setenv("STEMMA_TEST_PROJECT", "catalog")
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: '${STEMMA_TEST_PROJECT}'}
spec: {imports: [software.yaml]}
`)
	writeConfig(t, root, "software.yaml", `apiVersion: stemma/v1alpha1
kind: Software
metadata: {name: app}
spec: {source: {type: file, path: app.pkg}}
`)
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Fatalf("project identity depends on runner environment: %v", err)
	}
	if _, err := FindRoot(root); err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Fatalf("discovery accepted a different identity contract: %v", err)
	}
}
