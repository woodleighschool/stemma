package engine

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/testutil/testproject"
)

// environmentProject reads env values in each place a command can use them:
// a build input, destination metadata, destination connection settings and
// reconcile's source control.
const environmentProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: environment}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: "{{ env.STEMMA_TEST_REPO }}"}}
  reconcile:
    source_control:
      type: github
      config: {private_key: "{{ env.STEMMA_TEST_KEY }}"}
---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata: {name: vendor}
spec:
  inputs:
    vendor: {path: "{{ env.STEMMA_TEST_VENDOR }}"}
  package:
    identifier: org.example.vendor
    version: "1.0"
  payload:
    /Applications/Vendor.app: {$input: vendor}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: vendor}
spec:
  source:
    resource: {kind: BuildMacPkg, name: vendor}
  destinations:
    repo:
      pkginfo:
        description: "{{ env.STEMMA_TEST_DESCRIPTION }}"
`

var environmentValues = map[string]string{
	"STEMMA_TEST_REPO":        "repo",
	"STEMMA_TEST_KEY":         "fixture-key",
	"STEMMA_TEST_VENDOR":      "Vendor.app",
	"STEMMA_TEST_DESCRIPTION": "Fixture vendor",
}

func environmentFixture(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, "Vendor.app"), os.DirFS("../apple/testdata/Fixture.app")); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, environmentProject)
	return Options{ConfigPath: filename, CacheDir: t.TempDir()}
}

// setEnvironment sets every fixture value except the missing ones.
func setEnvironment(t *testing.T, missing ...string) {
	t.Helper()
	for name, value := range environmentValues {
		t.Setenv(name, value)
		if slices.Contains(missing, name) {
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestValidateReadsNoEnvironment(t *testing.T) {
	opts := environmentFixture(t)
	setEnvironment(t, slices.Collect(maps.Keys(environmentValues))...)
	p, err := ValidateProject(t.Context(), opts, false)
	if err != nil {
		t.Fatalf("validation needed environment values: %v", err)
	}
	if p.Destinations["repo"].Config["path"] != "{{ env.STEMMA_TEST_REPO }}" {
		t.Fatal("validation evaluated connection settings")
	}
}

func TestResolvedValidationReadsEveryEnvironmentValue(t *testing.T) {
	opts := environmentFixture(t)
	for _, name := range slices.Sorted(maps.Keys(environmentValues)) {
		t.Run(name, func(t *testing.T) {
			setEnvironment(t, name)
			if _, err := ValidateProject(t.Context(), opts, true); err == nil || !strings.Contains(err.Error(), "required reference is missing") {
				t.Fatalf("resolved validation without %s: %v", name, err)
			}
		})
	}
	setEnvironment(t)
	p, err := ValidateProject(t.Context(), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Destinations["repo"].Config["path"] != "repo" || p.Reconcile.SourceControl.Config["private_key"] != "fixture-key" {
		t.Fatalf("resolved project kept connection expressions: %+v", p)
	}
}

func TestCommandsReadEnvironmentValuesWhereTheyUseThem(t *testing.T) {
	opts := environmentFixture(t)
	opts.Resources = []string{"MacSoftware/vendor"}
	run := func(method string, missing ...string) error {
		t.Helper()
		setEnvironment(t, missing...)
		opts.Method = method
		_, err := Run(t.Context(), opts)
		return err
	}
	if err := run("update", "STEMMA_TEST_VENDOR"); err == nil || !strings.Contains(err.Error(), "required reference is missing") {
		t.Fatalf("acquisition without its input value: %v", err)
	}
	// Acquisition and preparation read neither publication metadata nor
	// connection settings, and only reconcile reads source control.
	if err := run("update", "STEMMA_TEST_DESCRIPTION", "STEMMA_TEST_REPO", "STEMMA_TEST_KEY"); err != nil {
		t.Fatalf("acquisition read publication values: %v", err)
	}
	if err := run("prepare", "STEMMA_TEST_DESCRIPTION", "STEMMA_TEST_REPO", "STEMMA_TEST_KEY"); err != nil {
		t.Fatalf("preparation read publication values: %v", err)
	}
	for _, missing := range []string{"STEMMA_TEST_DESCRIPTION", "STEMMA_TEST_REPO"} {
		if err := run("plan", missing); err == nil || !strings.Contains(err.Error(), "required reference is missing") {
			t.Fatalf("publication without %s: %v", missing, err)
		}
	}
	if err := run("plan", "STEMMA_TEST_KEY"); err != nil {
		t.Fatalf("publication with every value it uses: %v", err)
	}
}
