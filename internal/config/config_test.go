package config

import (
	"strings"
	"testing"
)

func TestCompositionRetainsPresence(t *testing.T) {
	p, err := Parse([]byte(`version: 1
project: test
components:
  base:
    source: {type: http, url: https://example.test/a.pkg}
    destinations:
      repo: {description: inherited, unattended_install: true, catalogs: [testing]}
recipes:
  app:
    extends: base
    destinations:
      repo: {description: null, unattended_install: false, catalogs: []}
destinations:
  repo: {type: munki, path: repo}
`))
	if err != nil {
		t.Fatal(err)
	}
	metadata := p.Recipes["app"].Destinations["repo"]
	if value, present := metadata["description"]; !present || value != nil {
		t.Fatalf("null collapsed: %#v", metadata)
	}
	if metadata["unattended_install"] != false {
		t.Fatalf("false lost: %#v", metadata)
	}
	if len(metadata["catalogs"].([]any)) != 0 {
		t.Fatalf("list not replaced: %#v", metadata)
	}
	before := Fingerprint(p.Recipes["app"].Source)
	metadata["description"] = "edited"
	if before != Fingerprint(p.Recipes["app"].Source) {
		t.Fatal("metadata invalidated source identity")
	}
}

func TestRejectMalformedConfiguration(t *testing.T) {
	base := "version: 1\nproject: test\nrecipes:\n  app:\n    source: {type: http, url: https://example.test/a.pkg}\n"
	for name, document := range map[string]string{
		"unknown":     base + "typo: true\n",
		"duplicate":   base + "project: duplicate\n",
		"documents":   base + "---\nversion: 1\n",
		"credentials": strings.ReplaceAll(base, "https://example.test/a.pkg", "https://user:password@example.test/a.pkg"),
		"cycle":       "version: 1\nproject: test\ncomponents:\n  a: {extends: b}\n  b: {extends: a}\nrecipes:\n  app: {extends: a}\n",
		"nonfinite":   base + "destinations:\n  intune: {type: intune, config: {value: .nan}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(document)); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}
