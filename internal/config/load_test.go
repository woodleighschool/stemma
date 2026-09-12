package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/testproject"
)

const projectFixture = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: catalog
spec:
  imports:
    - '*.software.yaml'
`
const resourceFixture = `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: app
spec:
  source:
    path: vendor.pkg
`

func TestLoadResourceFamiliesAndDiscoverRoot(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", strings.Replace(projectFixture, "'*.software.yaml'", "software/**/*.yaml", 1)+`  components:
    mac:
      arch: universal
`)
	writeConfigStream(t, root, "software/Branding/stemma.yaml", `apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata:
  name: branding
spec:
  inputs:
    payload:
      path: Payload
  package:
    identifier: org.example.branding
    version: "1.0"
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: branding
spec:
  extends: mac
  source:
    resource:
      kind: BuildMacPkg
      name: branding
---
apiVersion: stemma/v1alpha1
kind: WindowsSoftware
metadata:
  name: branding
spec:
  source:
    path: ../Shared/setup.exe
  destinations:
    endpoint:
      displayName: Branding
`)
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Resources) != 3 {
		t.Fatal("family lost distinct resource kinds sharing one name")
	}
	for _, resource := range p.Resources {
		if resource.Base != "software/Branding" {
			t.Fatal("resource lost its authoring directory")
		}
	}
	if p.Resources["stemma/v1alpha1/MacSoftware/branding"].Spec["arch"] != "universal" || p.Resources["stemma/v1alpha1/WindowsSoftware/branding"].Spec["source"].(map[string]any)["path"] != "../Shared/setup.exe" {
		t.Fatal("composition or opaque resolver declaration changed")
	}
	if found, err := FindRoot(filepath.Join(root, "software", "Branding")); err != nil || found != root {
		t.Fatalf("root discovery: %q, %v", found, err)
	}
}

func TestLoadExternalResourceEnvelope(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", strings.Replace(projectFixture, "'*.software.yaml'", "family/stemma.yaml", 1))
	writeConfig(t, root, "family/stemma.yaml", `apiVersion: example.test/v2
kind: Transform
metadata:
  name: fixture
spec:
  custom:
    nested:
      - false
      - null
`)
	p, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil || len(p.Resources) != 1 || p.Resources["example.test/v2/Transform/fixture"].Spec["custom"] == nil {
		t.Fatalf("external kind was not preserved for registry validation: %v", err)
	}
	if found, err := FindRoot(filepath.Join(root, "family")); err != nil || found != root {
		t.Fatalf("external family root discovery: %q, %v", found, err)
	}
}

func TestRejectInvalidFamilyImports(t *testing.T) {
	for _, pattern := range []string{"missing/**/stemma.yaml", "../outside.yaml", "software/[", "stemma.yaml"} {
		t.Run(pattern, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, "stemma.yaml", strings.Replace(projectFixture, "*.software.yaml", pattern, 1))
			if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
				t.Fatal("accepted invalid import")
			}
		})
	}
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", projectFixture)
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte(resourceFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside.software.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
		t.Fatal("import escaped the project through a symlink")
	}
}

func TestResourceIdentitySurvivesFileRename(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", projectFixture)
	writeConfig(t, root, "first.software.yaml", resourceFixture)
	before, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "first.software.yaml"), filepath.Join(root, "renamed.software.yaml")); err != nil {
		t.Fatal(err)
	}
	after, err := Load(filepath.Join(root, "stemma.yaml"))
	if err != nil || Fingerprint(before) != Fingerprint(after) {
		t.Fatalf("file rename changed resource identity: %v", err)
	}
}

func TestRejectMalformedFamilyStreams(t *testing.T) {
	project := strings.Replace(projectFixture, "'*.software.yaml'", "software/stemma.yaml", 1)
	for name, stream := range map[string]string{
		"empty": "", "comments-only": "# no resources\n", "null": "null\n",
		"empty-first":      "---\n---\n" + resourceFixture,
		"empty-last":       resourceFixture + "---\n",
		"empty-middle":     resourceFixture + "---\n---\n" + strings.Replace(resourceFixture, "name: app", "name: other", 1),
		"null-last":        resourceFixture + "---\nnull\n",
		"missing-spec":     strings.Split(resourceFixture, "spec:")[0],
		"null-spec":        strings.Split(resourceFixture, "spec:")[0] + "spec: null\n",
		"malformed-last":   resourceFixture + "---\nspec:\n  source: [\n",
		"unknown-last":     resourceFixture + "---\n" + strings.Replace(resourceFixture, "name: app", "name: other", 1) + "unknown: true\n",
		"project-last":     resourceFixture + "---\n" + project,
		"project-only":     project,
		"duplicate":        resourceFixture + "---\n" + resourceFixture,
		"unknown-envelope": resourceFixture + "unknown: true\n",
		"invalid-version":  strings.Replace(resourceFixture, "stemma/v1alpha1", "invalid", 1),
		"invalid-name":     strings.Replace(resourceFixture, "name: app", "name: ../app", 1),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, "stemma.yaml", project)
			writeConfigStream(t, root, "software/stemma.yaml", stream)
			if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
				t.Fatal("loaded invalid family stream")
			}
			// A well-formed nested Project is itself a discovery root, while
			// Project documents are never allowed at an import boundary.
			if name != "project-only" {
				if _, err := FindRoot(filepath.Join(root, "software")); err == nil {
					t.Fatal("discovery skipped invalid family stream")
				}
			}
		})
	}
	t.Run("project-stream", func(t *testing.T) {
		root := t.TempDir()
		writeConfigStream(t, root, "stemma.yaml", project+"---\n"+resourceFixture)
		if _, err := Load(filepath.Join(root, "stemma.yaml")); err == nil {
			t.Fatal("loaded a multi-document Project file")
		}
		if _, err := FindRoot(root); err == nil {
			t.Fatal("discovered a multi-document Project file")
		}
	})
}

func TestFindRootRejectsMalformedIntermediateProject(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", projectFixture)
	for _, document := range []string{projectFixture + "unknown: true\n", strings.Replace(projectFixture, "stemma/v1alpha1", "example.test/v2", 1)} {
		writeConfig(t, root, "nested/stemma.yaml", document)
		if _, err := FindRoot(filepath.Join(root, "nested")); err == nil {
			t.Fatal("accepted malformed nearer Project")
		}
	}
}

func writeConfig(t *testing.T, root, name, document string) {
	t.Helper()
	writeConfigStream(t, root, name, document)
}

func writeConfigStream(t *testing.T, root, name, document string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(document), 0o600); err != nil {
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
