package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
)

func TestResolveReportsEachResourceWithoutWritingTheLock(t *testing.T) {
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture.pkg" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(installer)
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	if err := os.WriteFile(filepath.Join(root, "branding.txt"), []byte("fixture"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: candidates
spec:
  imports:
    - '*.software.yaml'
  destinations:
    repo:
      operation: munki
      config:
        path: repo
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: alpha
spec:
  source:
    url: ` + server.URL + `/fixture.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: broken
spec:
  source:
    url: ` + server.URL + `/missing.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata:
  name: branding
spec:
  inputs:
    image:
      path: branding.txt
  payload:
    /Library/Example/branding.txt:
      $input: image
      mode: '0644'
  package:
    identifier: edu.example.branding
    version: '1.0'
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: branding
spec:
  source:
    resource:
      kind: BuildMacPkg
      name: branding
      output: installer
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: private
suspend: true
spec:
  source:
    resource:
      kind: BuildMacPkg
      name: branding
      output: installer
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
`
	testproject.Write(t, filename, manifest)
	reported := map[string]ResourceReport{}
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), ResourceDone: func(resource ResourceReport) error {
		reported[resource.Key] = resource
		return nil
	}}
	candidate, err := Resolve(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockfile.Filename(root)); !os.IsNotExist(err) {
		t.Fatal("candidate resolution wrote the lockfile")
	}
	if candidate.Lock.Version != 0 || len(candidate.Resources) != 5 {
		t.Fatalf("unexpected candidate: %+v", candidate)
	}
	const alpha, broken, build, consumer = "stemma/v1alpha1/MacSoftware/alpha", "stemma/v1alpha1/MacSoftware/broken", "stemma/v1alpha1/BuildMacPkg/branding", "stemma/v1alpha1/MacSoftware/branding"
	if entry := candidate.Resources[alpha].Inputs["source"]; entry.Content.Filename != "fixture.pkg" || candidate.Resources[alpha].Error != "" {
		t.Fatalf("reachable source was not resolved: %+v", candidate.Resources[alpha])
	}
	if resource := candidate.Resources[broken]; resource.Inputs != nil || !strings.Contains(resource.Error, "HTTP 404") {
		t.Fatalf("unreachable source did not fail independently: %+v", resource)
	}
	if resource := candidate.Resources[build]; len(resource.Inputs) != 1 || len(resource.Producers) != 0 {
		t.Fatalf("local inputs were not observed: %+v", resource)
	}
	if resource := candidate.Resources[consumer]; len(resource.Inputs) != 0 || len(resource.Producers) != 1 || resource.Producers[0] != build {
		t.Fatalf("output reference was not recorded as a producer: %+v", resource)
	}
	const private = "stemma/v1alpha1/MacSoftware/private"
	if resource := candidate.Resources[private]; !resource.Suspended || resource.Inputs != nil || resource.Error != "" || len(resource.Producers) != 0 {
		t.Fatalf("suspended resource was evaluated or resolved: %+v", resource)
	}
	// Each resource reports its lock changes as the lookup finishes it.
	if len(reported) != 4 || len(reported[alpha].Inputs) != 1 || len(reported[build].Inputs) != 1 || len(reported[consumer].Inputs) != 0 || !strings.Contains(reported[broken].Error, "HTTP 404") {
		t.Fatalf("resolution reports: %+v", reported)
	}
	if dependents := candidate.Dependents(build); len(dependents) != 1 || dependents[0] != consumer {
		t.Fatalf("dependents of the build: %v", dependents)
	}
	if dependents := candidate.Dependents(consumer); len(dependents) != 0 {
		t.Fatalf("leaf resource has dependents: %v", dependents)
	}
	options.Method = "update"
	options.Resources = []string{"MacSoftware/alpha"}
	options.ResourceDone = nil
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	reviewed, err := lockfile.Load(lockfile.Filename(root))
	if err != nil {
		t.Fatal(err)
	}
	clear(reported)
	again, err := Resolve(t.Context(), Options{ConfigPath: filename, CacheDir: options.CacheDir, ResourceDone: func(resource ResourceReport) error {
		reported[resource.Key] = resource
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Lock.Version != lockfile.Version || !again.Resources[alpha].Inputs["source"].Equal(reviewed.Inputs[alpha]["source"]) || len(reported[alpha].Inputs) != 0 {
		t.Fatalf("unchanged bytes changed the candidate entry: %+v", again.Resources[alpha].Inputs["source"])
	}
}
