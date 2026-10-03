package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
)

func TestInspectInputReportsAddressableFactsWithoutBuilding(t *testing.T) {
	root, fixture := t.TempDir(), t.TempDir()
	iconFixtures(t, fixture)
	data, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(fixture, "Installers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "Installers", "Example.pkg"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "vendor.zip")
	testarchive.Zip(t, archive, fixture)
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: inspection}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata: {name: wrapper}
spec:
  inputs:
    vendor: {path: vendor.zip}
    unrelated: {path: missing.dat}
  package:
    identifier: org.example.wrapper
    version: "{{ inputs.vendor.facts['missing.app'].app.version }}"
  scripts: {postinstall: '#!/bin/sh'}
`)
	opts := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "inspect", Resources: []string{"BuildMacPkg/wrapper"}, Input: InputSelection{Name: "vendor"}, Lock: lockfile.Options{IgnoreInputs: true}}
	for _, selection := range []string{"", "Example.app", "Installers/Example.pkg"} {
		t.Run(selection, func(t *testing.T) {
			opts.Input.Path = selection
			report, err := Run(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			local, err := Inspect(t.Context(), archive, selection)
			if err != nil {
				t.Fatal(err)
			}
			facts := report.Inspection.Facts
			if len(facts.Subjects) != len(local.Facts.Subjects) {
				t.Fatal("local and input inventories differ")
			}
			for i, subject := range facts.Subjects {
				if subject.ID != local.Facts.Subjects[i].ID || subject.Package != nil {
					t.Fatalf("unaddressable fact: %+v", subject)
				}
			}
			if selection == "Example.app" && (len(facts.Subjects) != 1 || facts.Subjects[0].App.Version != "1.2" || facts.Subjects[0].App.Build != "123") {
				t.Fatalf("app facts: %+v", facts)
			}
			if selection == "Installers/Example.pkg" && (len(facts.Subjects) != 1 || facts.Subjects[0].Kind != "container") {
				t.Fatalf("package inventory: %+v", facts)
			}
			if len(report.Resources) != 1 || len(report.Resources[0].Artifacts) != 0 {
				t.Fatalf("inspection prepared a resource: %+v", report.Resources)
			}
		})
	}
	opts.Input.Path = "Missing.app"
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), `available subject IDs: ["." "Example.app" "Installers/Example.pkg"]`) {
		t.Fatalf("missing subject: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "stemma.lock.yaml")); !os.IsNotExist(err) {
		t.Fatalf("inspection wrote lock: %v", err)
	}
}

func TestLocalInspectionRetainsContainerDigest(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "notice.txt"), []byte("synthetic content"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"zip", "dmg"} {
		t.Run(format, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "vendor."+format)
			if format == "zip" {
				testarchive.Zip(t, filename, source)
			} else {
				testdiskimage.Write(t, filename, source)
			}
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			result, err := Inspect(t.Context(), filename, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Facts.Subjects[0].SHA256; got != hex.EncodeToString(digest[:]) {
				t.Fatalf("container digest = %q", got)
			}
		})
	}
}
