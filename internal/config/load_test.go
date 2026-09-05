package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSoftwareFamiliesAndDiscoverRoot(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", `version: 1
project: software
imports: [software/**/stemma.yaml]
components:
  mac: {platform: darwin, arch: universal}
destinations:
  repo: {type: munki, path: repo}
`)
	writeConfig(t, root, "software/Branding/stemma.yaml", `version: 1
recipes:
  branding:
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
      repo: {artifact: package, catalogs: [testing]}
  portal:
    source: {type: http, url: "https://go.microsoft.com/fwlink/?linkid=853070", filename: CompanyPortal.pkg}
`)
	writeConfig(t, root, "software/Other/stemma.yaml", `version: 1
recipes:
  other:
    source: {type: file, path: ../Shared/vendor.pkg}
`)
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	branding := p.Recipes["branding"]
	if len(p.Recipes) != 3 || branding.Platform != "darwin" || branding.Source.Base != "software/Branding" || p.Recipes["other"].Source.Path != "software/Shared/vendor.pkg" {
		t.Fatalf("families or relative paths did not resolve: %#v", p.Recipes)
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
		"unknown-field":   {"software/App/stemma.yaml", "version: 1\nrecipes: {}\ncomponents: {}\n"},
		"bad-version":     {"software/App/stemma.yaml", "version: 2\nrecipes: {app: {source: {type: file, path: app.pkg}}}\n"},
		"parent-source":   {"software/App/stemma.yaml", "version: 1\nrecipes: {app: {source: {type: file, path: ../../../app.pkg}}}\n"},
		"empty-fragment":  {"software/App/stemma.yaml", "version: 1\nrecipes: {}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, "stemma.yaml", "version: 1\nproject: test\nimports: ['"+test.pattern+"']\n")
			if test.fragment != "" {
				writeConfig(t, root, "software/App/stemma.yaml", test.fragment)
			}
			if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
				t.Fatal("accepted invalid import")
			}
		})
	}
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", "version: 1\nproject: test\nimports: [software/**/stemma.yaml]\nrecipes: {app: {source: {type: file, path: app.pkg}}}\n")
	writeConfig(t, root, "software/App/stemma.yaml", "version: 1\nrecipes: {app: {source: {type: file, path: different.pkg}}}\n")
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil || !strings.Contains(err.Error(), "conflicting recipe ID") {
		t.Fatalf("duplicate recipe ID: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "software", "App", "stemma.yaml")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("version: 1\nrecipes: {outside: {source: {type: file, path: app.pkg}}}\n"), 0o600); err != nil {
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
	base := `version: 1
project: test
recipes:
  app:
    source: {type: local, include: [Payload/**]}
    artifacts:
      package: {type: pkg, identifier: org.example.app, version: "1.0", payload: Payload}
    destinations: {repo: {artifact: package}}
destinations: {repo: {type: munki, path: repo}}
`
	for _, change := range [][2]string{{"type: pkg", "type: app"}, {"payload: Payload", "payload: ../outside"}, {"artifact: package", "artifact: missing"}, {"identifier: org.example.app", "identifier: bad/id"}, {"version: \"1.0\"", "version: \"\""}, {"payload: Payload", "scripts: {uninstall: Scripts/postinstall}"}, {"payload: Payload", "payload: Payload, filename: ../app.pkg"}} {
		if _, err := Parse([]byte(strings.Replace(base, change[0], change[1], 1))); err == nil {
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
	writeConfig(t, root, "stemma.yaml", "version: 1\nproject: root\nrecipes: {app: {source: {type: file, path: app.pkg}}}\n")
	writeConfig(t, root, "software/App/stemma.yaml", "version: 1\nproject: child\nunknown: true\n")
	if _, err := FindRoot(filepath.Join(root, "software", "App")); err == nil {
		t.Fatal("skipped malformed nearer project")
	}
}

func writeConfig(t *testing.T, root, name, document string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}
