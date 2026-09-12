package engine

import (
	"github.com/woodleighschool/stemma/internal/cas"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/testproject"
)

func TestBuildReferencesShareLockedInputsAndPreserveMetadataOnlyCache(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	if err := os.WriteFile(filepath.Join(root, "branding.txt"), []byte("Woodleigh fixture"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "postinstall"), []byte("#!/bin/sh\ntouch never-run-on-builder\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: layout
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
kind: BuildMacPkg
metadata:
  name: branding
spec:
  inputs:
    image:
      path: branding.txt
    script:
      path: postinstall
  payload:
    /Library/Example/branding.txt:
      $input: image
      mode: '0644'
      uid: 0
      gid: 0
  package:
    identifier: edu.example.branding
    version: '1.0'
  scripts:
    postinstall:
      $input: script
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
        description: original
`
	write := func(text string) {
		t.Helper()
		if err := testproject.Write(filename, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	write(manifest)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "apply"
	options.Resources = []string{"MacSoftware/branding"}
	first, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Resources) != 2 || first.Resources[0].Kind != "BuildMacPkg" || len(first.Resources[1].Destinations) != 1 {
		t.Fatalf("build dependency not prepared: %+v", first)
	}
	original := first.Resources[0].Artifacts["installer"].Payload
	write(strings.Replace(manifest, "description: original", "description: edited", 1))
	edited, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range edited.Resources {
		if !resource.Cached {
			t.Fatal("native metadata rebuilt immutable content")
		}
	}
	if err := os.RemoveAll(options.CacheDir); err != nil {
		t.Fatal(err)
	}
	cold, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if cold.Resources[0].Artifacts["installer"].Payload != original || len(cold.Resources[1].Destinations[0].Changes) != 0 {
		t.Fatal("cache loss replayed package publication")
	}
	if _, err := os.Stat(filepath.Join(root, "never-run-on-builder")); !os.IsNotExist(err) {
		t.Fatal("endpoint script executed on builder")
	}
	if err := os.WriteFile(filepath.Join(root, "branding.txt"), []byte("changed fixture"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "local input content changed") {
		t.Fatalf("locked input changed silently: %v", err)
	}
	options.Method = "update"
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "prepare"
	updated, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Resources[0].Artifacts["installer"].Payload == original {
		t.Fatal("changed payload reused previous package")
	}
}

func TestResourceFileImportRejectsUnrepresentableMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode test")
	}
	work := t.TempDir()
	filename := filepath.Join(work, "payload")
	if err := os.WriteFile(filename, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importPath(t.Context(), store, filename, false, work); err == nil {
		t.Fatal("resource artifact silently discarded setuid metadata")
	}
}
