package config

import (
	"strings"
	"testing"
)

func TestPluginOCIReferences(t *testing.T) {
	for _, test := range []struct {
		image          string
		trusted, valid bool
	}{
		{"ghcr.io/example/plugin:v1", true, true},
		{"ghcr.io/example/plugin@sha256:" + strings.Repeat("a", 64), true, true},
		{"ghcr.io/example/plugin", true, false},
		{"https://ghcr.io/example/plugin:v1", true, false},
		{"ghcr.io/example/plugin:v1", false, false},
	} {
		t.Run(test.image, func(t *testing.T) {
			project := Project{Project: "test", Software: map[string]Software{"fixture": {Source: &Source{Type: "file", Path: "fixture.pkg"}}}, Plugins: map[string]Plugin{"fixture": {Image: test.image, Trusted: test.trusted}}}
			if err := project.Validate(); (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
	if _, err := parseTest(t, []byte("apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: test\nspec:\n  plugins:\n    fixture:\n      trusted: true\n      platforms: {linux/amd64: {type: file, path: plugin}}\n  imports: ['*.software.yaml']\n---\napiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: fixture\nspec:\n  source: {type: file, path: fixture.pkg}\n")); err == nil {
		t.Fatal("removed raw executable configuration was accepted")
	}
}

func TestCompositionRetainsPresence(t *testing.T) {
	p, err := parseTest(t, []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: test
spec:
  components:
    base:
      source: {type: http, url: 'https://example.test/a.pkg'}
      destinations:
        repo: {pkginfo: {description: inherited, unattended_install: true, catalogs: [testing]}}
  destinations:
    repo: {operation: munki, config: {path: repo}}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: app
spec:
  extends: base
  destinations:
    repo: {pkginfo: {description: null, unattended_install: false, catalogs: []}}
`))
	if err != nil {
		t.Fatal(err)
	}
	metadata := p.Software["app"].Destinations["repo"]["pkginfo"].(map[string]any)
	if value, present := metadata["description"]; !present || value != nil {
		t.Fatalf("null collapsed: %#v", metadata)
	}
	if metadata["unattended_install"] != false {
		t.Fatalf("false lost: %#v", metadata)
	}
	if len(metadata["catalogs"].([]any)) != 0 {
		t.Fatalf("list not replaced: %#v", metadata)
	}
	before := Fingerprint(p.Software["app"].Source)
	metadata["description"] = "edited"
	if before != Fingerprint(p.Software["app"].Source) {
		t.Fatal("metadata invalidated source identity")
	}
}

func TestRejectMalformedConfiguration(t *testing.T) {
	base := "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: test\nspec:\n  imports: ['*.software.yaml']\n---\napiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: app\nspec:\n  source: {type: http, url: 'https://example.test/a.pkg'}\n"
	for name, document := range map[string]string{
		"unknown":          base + "typo: true\n",
		"duplicate":        base + "kind: Software\n",
		"documents":        base + "---\nversion: 1\n",
		"credentials":      strings.ReplaceAll(base, "https://example.test/a.pkg", "https://user:password@example.test/a.pkg"),
		"cycle":            "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: test\nspec:\n  components:\n    a: {extends: b}\n    b: {extends: a}\n  imports: ['*.software.yaml']\n---\napiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: app\nspec: {extends: a}\n",
		"nonfinite":        strings.Replace(base, "  imports:", "  destinations: {intune: {operation: intune, config: {value: .nan}}}\n  imports:", 1),
		"source-version":   strings.Replace(base, "type: http", "version: '1.0', type: http", 1),
		"destination-type": strings.Replace(base, "  imports:", "  destinations: {repo: {type: munki, path: repo}}\n  imports:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTest(t, []byte(document)); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestOperationsAndSubjectReferences(t *testing.T) {
	base := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: test
spec:
  destinations:
    external: {operation: vendor.publish, config: {token: test-token}}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: app
spec:
  source: {type: local, include: [Payload/**]}
  subjects:
    main: {kind: app, bundle_id: org.example.app, installed_path: /Applications/App.app}
  artifacts:
    package: {type: pkg, identifier: org.example.app, version: "1.0", payload: Payload}
  steps:
    - name: inspect
      operation: inspect
      inputs: {input: source}
    - name: transform
      operation: vendor.prepare
      inputs: {original: prepared, package: artifacts/package, extra: inspect/payload}
      config: {keep: false, omit: null, list: []}
  destinations:
    external: {installer: transform/package, inputs: {installer: source, package: artifacts/package},
      version: {$fact: main.app.version}}
`
	p, err := parseTest(t, []byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if p.Destinations["external"].Operation != "vendor.publish" || p.Software["app"].Steps[1].Inputs["extra"] != "inspect/payload" {
		t.Fatal("operation names or references were not retained")
	}
	for name, change := range map[string][2]string{
		"duplicate":                {"name: transform", "name: inspect"},
		"reserved":                 {"name: inspect", "name: source"},
		"missing-operation":        {"operation: inspect", "operation: ''"},
		"uppercase-operation":      {"operation: vendor.publish", "operation: Vendor.publish"},
		"unsafe-operation":         {"operation: vendor.prepare", "operation: vendor/../prepare"},
		"forward":                  {"input: source", "input: transform/package"},
		"self":                     {"input: source", "input: inspect/payload"},
		"unknown-step":             {"extra: inspect/payload", "extra: missing/payload"},
		"unknown-artifact":         {"package: artifacts/package", "package: artifacts/missing"},
		"extra-segment":            {"extra: inspect/payload", "extra: inspect/payload/child"},
		"bare-artifact":            {"installer: transform/package", "installer: package"},
		"non-object-inputs":        {"inputs: {installer: source, package: artifacts/package}", "inputs: [source]"},
		"non-string-input":         {"installer: source", "installer: 4"},
		"unknown-input-output":     {"installer: source", "installer: missing/output"},
		"unsafe-destination-input": {"installer: source", "../installer: source"},
		"missing-output":           {"extra: inspect/payload", "extra: inspect/"},
		"unsafe-input":             {"extra: inspect/payload", "../extra: inspect/payload"},
		"empty-subject":            {"kind: app, bundle_id: org.example.app, installed_path: /Applications/App.app", ""},
		"dotted-subject":           {"main: {kind", "main.app: {kind"},
		"uppercase-subject":        {"main: {kind", "Main: {kind"},
		"relative-installed-path":  {"installed_path: /Applications/App.app", "installed_path: Applications/App.app"},
		"escaping-subject-path":    {"kind: app", "path: ../App.app"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTest(t, []byte(strings.Replace(base, change[0], change[1], 1))); err == nil {
				t.Fatal("accepted invalid operation or subject reference")
			}
		})
	}
	for _, operation := range []string{"vendor/prepare", "vendor.prepare", "vendor_prepare", "vendor-prepare"} {
		t.Run(operation, func(t *testing.T) {
			if _, err := parseTest(t, []byte(strings.Replace(base, "operation: vendor.prepare", "operation: "+operation, 1))); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, selector := range []string{"source", "prepared", "artifacts/package", "inspect/payload", "transform/package"} {
		t.Run(selector, func(t *testing.T) {
			if _, err := parseTest(t, []byte(strings.Replace(base, "installer: transform/package", "installer: "+selector, 1))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerificationArtifactReferences(t *testing.T) {
	base := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: verify
spec:
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: app
spec:
  source: {type: local, include: [Payload/**]}
  verification: {subject: SUBJECT, integrity: true}
  artifacts:
    package: {type: pkg, identifier: org.example.app, version: "1", payload: Payload}
  steps:
    - name: build
      operation: pkg
      inputs: {input: prepared}
      config: {identifier: org.example.app, version: "1", payload: Payload}
`
	for _, subject := range []string{"source", "payload", "prepared", "artifacts/package", "build/artifact"} {
		if _, err := parseTest(t, []byte(strings.Replace(base, "SUBJECT", subject, 1))); err != nil {
			t.Fatalf("supported verification reference %s: %v", subject, err)
		}
	}
	for _, subject := range []string{"package", "artifacts/missing", "missing/artifact", "build/", "build/artifact/child"} {
		if _, err := parseTest(t, []byte(strings.Replace(base, "SUBJECT", subject, 1))); err == nil {
			t.Fatalf("invalid verification reference %s was accepted", subject)
		}
	}
}
