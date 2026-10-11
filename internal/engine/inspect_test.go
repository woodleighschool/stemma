package engine

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
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
	if _, err := os.Stat(lockfile.Path(root)); !os.IsNotExist(err) {
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

// A member is read from the stored object, whether it is the prepared
// artifact, a locked input or a package inside that input's disk image.
func TestReadWritesOneFileInsideAnArtifact(t *testing.T) {
	root, fixture := t.TempDir(), t.TempDir()
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(fixture, "Installers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "Installers", "Example.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(root, "vendor.dmg")
	testdiskimage.Write(t, image, fixture)
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: members}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: app}
spec:
  source: {path: vendor.dmg}
  package_path: Installers/Example.pkg
`)
	cache := t.TempDir()
	options := Options{ConfigPath: filename, CacheDir: cache, Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	const identifier = `identifier="au.edu.vic.woodleigh.stemma.fixture"`
	for _, test := range []struct {
		name, read, want, failure string
		input                     string
	}{
		{name: "artifact", read: "PackageInfo", want: identifier},
		{name: "artifact directory", read: ".", want: "Bom\nPackageInfo\nPayload/\n"},
		{name: "artifact miss", read: "Distribution", failure: `the package has no "Distribution"; it holds Bom, PackageInfo, Payload/`},
		{name: "input", input: "source", read: "Installers/Example.pkg/PackageInfo", want: identifier},
		{name: "input package", input: "source", read: "Installers/Example.pkg", want: "Bom\nPackageInfo\nPayload/\n"},
		{name: "input directory", input: "source", read: "Installers", want: "Example.pkg\n"},
		{name: "input miss", input: "source", read: "Installer/Example.pkg", failure: `the input has no "Installer"; it holds Installers/`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out strings.Builder
			options := options
			options.Method, options.Resources, options.Read, options.ReadTo = "artifact", []string{"MacSoftware/app"}, test.read, &out
			if test.input != "" {
				options.Method, options.Input = "inspect", InputSelection{Name: test.input}
			}
			report, err := Run(t.Context(), options)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("error = %v, want %q", err, test.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("read %q, want %q", out.String(), test.want)
			}
			if report.Artifact != "" || report.Inspection != nil {
				t.Fatalf("read also reported %+v", report)
			}
		})
	}
	if copies, err := os.ReadDir(filepath.Join(cache, "materialized")); err != nil || len(copies) != 0 {
		t.Fatalf("read materialized %d artifacts: %v", len(copies), err)
	}
	var out strings.Builder
	if err := ReadMember(t.Context(), image, "Installers/Example.pkg/PackageInfo", &out); err != nil || !strings.Contains(out.String(), identifier) {
		t.Fatalf("local read %q: %v", out.String(), err)
	}
}

// A read stays inside the input it was given. It accepts no absolute or parent
// path and follows no symlink, whether the link is the file it names or a
// directory above it, in a folder or in an archive.
func TestReadStaysInsideItsInput(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(tree, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "nested", "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(filepath.Join(tree, "nested"), secret)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"absolute": secret, "nested/relative": filepath.ToSlash(relative), "folder": outside}
	if err := os.Symlink(links["absolute"], filepath.Join(tree, "absolute")); err != nil {
		t.Skip(err)
	}
	for _, name := range []string{"nested/relative", "folder"} {
		if err := os.Symlink(filepath.FromSlash(links[name]), filepath.Join(tree, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	// The archive holds the same links as entries of its own.
	archive := filepath.Join(t.TempDir(), "tree.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	add := func(name, content string, mode fs.FileMode) {
		t.Helper()
		header := &zip.FileHeader{Name: name}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	add("nested/inside.txt", "inside", 0o644)
	for name, target := range links {
		add(name, target, fs.ModeSymlink|0o777)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{tree, archive} {
		for _, member := range []string{"absolute", "nested/relative", "folder/secret.txt", "../secret.txt", "nested/../../secret.txt", filepath.ToSlash(secret)} {
			var out strings.Builder
			if err := ReadMember(t.Context(), input, member, &out); err == nil || out.Len() != 0 {
				t.Errorf("%s: reading %q wrote %q: %v", filepath.Base(input), member, out.String(), err)
			}
		}
	}
	var out strings.Builder
	if err := ReadMember(t.Context(), tree, "nested/inside.txt", &out); err != nil || out.String() != "inside" {
		t.Fatalf("read %q inside the folder: %v", out.String(), err)
	}
}
