package config

import (
	"github.com/woodleighschool/stemma/internal/testproject"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSoftwareFamiliesAndDiscoverRoot(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: software
spec:
  imports: [software/**/*.yaml]
  components:
    mac: {platform: darwin, arch: universal}
  destinations:
    repo: {operation: munki, path: repo}
`)
	writeConfig(t, root, "software/Branding/stemma.yaml", `apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: branding
spec:
  extends: mac
  source: {type: local, include: [Payload/**, Scripts/**]}
  artifacts:
    package:
      type: pkg
      identifier: org.example.branding
      version: "1.0"
      payload: Payload
      scripts: {postinstall: Scripts/postinstall}
  destinations:
    repo: {artifact: artifacts/package, catalogs: [testing]}
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: portal
spec:
  source: {type: http, url: "https://go.microsoft.com/fwlink/?linkid=853070", filename: CompanyPortal.pkg}
`)
	writeConfig(t, root, "software/Other/stemma.yaml", `apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: other
spec:
  source: {type: file, path: ../Shared/vendor.pkg}
`)
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	branding := p.Software["branding"]
	if len(p.Software) != 3 || branding.Platform != "darwin" || branding.Source.Base != "software/Branding" || p.Software["other"].Source.Path != "software/Shared/vendor.pkg" {
		t.Fatalf("families or relative paths did not resolve: %#v", p.Software)
	}
	if branding.Artifacts["package"].Scripts["postinstall"] != "Scripts/postinstall" {
		t.Fatal("source-tree artifact path was rebased")
	}
	found, err := FindRoot(filepath.Join(root, "software", "Branding"))
	if err != nil || found != root {
		t.Fatalf("root discovery: %q, %v", found, err)
	}
}

func TestRejectInvalidFamilyImports(t *testing.T) {
	for name, test := range map[string]struct{ pattern, fragment string }{
		"missing-pattern": {"missing/**/stemma.yaml", ""},
		"parent-pattern":  {"../outside.yaml", ""},
		"bad-pattern":     {"software/[", ""},
		"unknown-field":   {"software/App/stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Software\nmetadata: {name: app}\nspec: {source: {type: file, path: app.pkg}}\ncomponents: {}\n"},
		"bad-version":     {"software/App/stemma.yaml", "apiVersion: stemma/v99\nkind: Software\nmetadata:\n  name: app\nspec: {source: {type: file, path: app.pkg}}\n"},
		"parent-source":   {"software/App/stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: app\nspec: {source: {type: file, path: ../../../app.pkg}}\n"},
		"empty-source":    {"software/App/stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Software\nmetadata: {name: app}\nspec: {source: {}}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, "stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata: {name: test}\nspec: {imports: ['"+test.pattern+"']}\n")
			if test.fragment != "" {
				writeConfig(t, root, "software/App/stemma.yaml", test.fragment)
			}
			if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
				t.Fatal("accepted invalid import")
			}
		})
	}
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: test\nspec:\n  imports: [software/**/stemma.yaml, '*.software.yaml']\n---\napiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: app\nspec: {source: {type: file, path: app.pkg}}\n")
	writeConfig(t, root, "software/App/stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: app\nspec: {source: {type: file, path: different.pkg}}\n")
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil || !strings.Contains(err.Error(), "conflicting software ID") {
		t.Fatalf("duplicate software ID: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "software", "App", "stemma.yaml")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("apiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: outside\nspec: {source: {type: file, path: app.pkg}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "software", "App", "stemma.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
		t.Fatal("import read outside project through symlink")
	}
}

func TestArtifactAndStableURLValidation(t *testing.T) {
	base := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: test
spec:
  destinations: {repo: {operation: munki, path: repo}}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: app
spec:
  source: {type: local, include: [Payload/**]}
  artifacts:
    package: {type: pkg, identifier: org.example.app, version: "1.0", payload: Payload}
  destinations: {repo: {artifact: artifacts/package}}
`
	for _, change := range [][2]string{{"type: pkg", "type: app"}, {"payload: Payload", "payload: ../outside"}, {"artifact: artifacts/package", "artifact: artifacts/missing"}, {"identifier: org.example.app", "identifier: bad/id"}, {"version: \"1.0\"", "version: \"\""}, {"payload: Payload", "scripts: {uninstall: Scripts/postinstall}"}, {"payload: Payload", "payload: Payload, filename: ../app.pkg"}, {"payload: Payload", "payload: Payload, from: source"}} {
		if _, err := parseTest(t, []byte(strings.Replace(base, change[0], change[1], 1))); err == nil {
			t.Fatalf("accepted invalid artifact change %v", change)
		}
	}
	for _, address := range []string{"https://go.microsoft.com/fwlink/?linkid=853070", "https://example.test/download?channel=stable&platform=mac"} {
		if err := ValidateHTTPURL(address); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"token=private", "X-Amz-Signature=private", "sig=private", "expires=123", "api_key=private"} {
		if err := ValidateHTTPURL("https://example.test/app?" + query); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("credential URL was accepted or leaked: %v", err)
		}
	}
}

func TestFindRootRejectsMalformedIntermediateFile(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: root\nspec:\n  imports: ['*.software.yaml']\n---\napiVersion: stemma/v1alpha1\nkind: Software\nmetadata:\n  name: app\nspec: {source: {type: file, path: app.pkg}}\n")
	writeConfig(t, root, "software/App/stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: child\nspec:\n  unknown: true\n")
	if _, err := FindRoot(filepath.Join(root, "software", "App")); err == nil {
		t.Fatal("skipped malformed nearer project")
	}
}

func TestSoftwareIdentitySurvivesFileRename(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", "apiVersion: stemma/v1alpha1\nkind: Project\nmetadata: {name: catalog}\nspec: {imports: ['*.software.yaml']}\n")
	document := "apiVersion: stemma/v1alpha1\nkind: Software\nmetadata: {name: app}\nspec: {source: {type: file, path: vendor.pkg}}\n"
	writeConfig(t, root, "first.software.yaml", document)
	before, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "first.software.yaml"), filepath.Join(root, "renamed.software.yaml")); err != nil {
		t.Fatal(err)
	}
	after, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil || Fingerprint(before) != Fingerprint(after) {
		t.Fatalf("file rename changed resolved identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "renamed.software.yaml"), []byte(document+"---\n"+document), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil || !strings.Contains(err.Error(), "one YAML document") {
		t.Fatalf("accepted multiple resources in one imported file: %v", err)
	}
}

func writeConfig(t *testing.T, root, name, document string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testproject.Write(filename, []byte(document)); err != nil {
		t.Fatal(err)
	}
}

func parseTest(t *testing.T, data []byte) (Project, error) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	if err := testproject.Write(filename, data); err != nil {
		return Project{}, err
	}
	return Load(filename)
}
