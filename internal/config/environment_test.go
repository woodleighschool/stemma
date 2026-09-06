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
			p, err := Parse([]byte(`version: 1
project: test
components:
  base:
    source: {type: http, url: https://example.test/app.pkg}
    destinations:
      repo:
        description: ${STEMMA_TEST_VALUE}
        catalogs: ["${STEMMA_TEST_VALUE}"]
recipes:
  app: {extends: base}
destinations:
  repo: {type: munki, path: repo}
  jamf:
    type: jamf
    config:
      client_secret: ${STEMMA_TEST_VALUE}
`))
			if err != nil {
				t.Fatal(err)
			}
			metadata := p.Recipes["app"].Destinations["repo"]
			if metadata["description"] != value || metadata["catalogs"].([]any)[0] != value || p.Destinations["jamf"].Config["client_secret"] != value {
				t.Fatal("environment value was changed, reinterpreted or not expanded")
			}
		})
	}
}

func TestEnvironmentRejectsInvalidPlaceholders(t *testing.T) {
	t.Setenv("STEMMA_TEST_MISSING", "")
	if err := os.Unsetenv("STEMMA_TEST_MISSING"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"${STEMMA_TEST_MISSING}", "prefix-${STEMMA_TEST_MISSING}", "${}", "${NAME:-default}", "${UNCLOSED"} {
		document := "version: 1\nproject: test\nrecipes:\n  app:\n    source:\n      type: http\n      url: https://example.test/app.pkg\n      token: '" + value + "'\n"
		if _, err := Parse([]byte(document)); err == nil || !strings.Contains(err.Error(), "environment") {
			t.Errorf("placeholder %q: %v", value, err)
		}
	}
}

func TestLoadExpandsImportedValuesWithoutRequiringEnvironmentForDiscovery(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", `version: 1
project: test
imports: [software/stemma.yaml]
destinations:
  jamf:
    type: jamf
    config:
      client_secret: ${STEMMA_TEST_SECRET}
`)
	writeConfig(t, root, "software/stemma.yaml", `version: 1
recipes:
  app:
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
	if p.Destinations["jamf"].Config["client_secret"] != "test-secret" || p.Recipes["app"].Source.Token != "test-secret" {
		t.Fatal("root or imported values were not expanded")
	}
	data, err := os.ReadFile(filepath.Join(root, "software/stemma.yaml"))
	if err != nil || !strings.Contains(string(data), "${STEMMA_TEST_SECRET}") {
		t.Fatal("loading rewrote the authored configuration")
	}
}

func TestEnvironmentDoesNotExpandKeysOrBypassStrictTypes(t *testing.T) {
	t.Setenv("STEMMA_TEST_VALUE", "private-test-value")
	base := "version: 1\nproject: test\nrecipes: {app: {source: {type: http, url: https://example.test/app.pkg}}}\n"
	for _, document := range []string{
		base + "destinations: {repo: {type: jamf, config: {'${STEMMA_TEST_VALUE}': value}}}\n",
		base + "unknown: '${STEMMA_TEST_VALUE}'\n",
		strings.Replace(base, "version: 1", "version: '${STEMMA_TEST_VALUE}'", 1),
		strings.Replace(base, "url: https://example.test/app.pkg", "url: https://example.test/app.pkg, token_env: OLD", 1),
	} {
		if _, err := Parse([]byte(document)); err == nil || strings.Contains(err.Error(), "private-test-value") {
			t.Fatalf("invalid configuration was accepted or exposed an environment value: %v", err)
		}
	}
}
