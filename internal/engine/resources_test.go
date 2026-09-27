package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestPreparationKeepsInputsImmutable(t *testing.T) {
	for _, change := range []string{"none", "content", "mode"} {
		t.Run(change, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ref, err := store.Import(t.Context(), strings.NewReader("original"), "")
			if err != nil {
				t.Fatal(err)
			}
			ops := &operations{registry: plugin.New("fixture", "1")}
			operation := plugin.Operation{Name: "fixture", Kind: "resource", SideEffects: "workspace", Methods: []string{"run"}, InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`)}
			err = ops.registry.Register(operation, func(_ context.Context, envelope plugin.Request) (plugin.Response, error) {
				var request plugin.ResourceRequest[json.RawMessage]
				if err := json.Unmarshal(envelope.Input, &request); err != nil {
					return plugin.Response{}, err
				}
				var err error
				switch change {
				case "content":
					err = os.WriteFile(request.Inputs["source"].Path, []byte("changed"), 0o640)
				case "mode":
					err = os.Chmod(request.Inputs["source"].Path, 0o444)
				}
				return plugin.Response{Output: json.RawMessage(`{}`)}, err
			})
			if err != nil {
				t.Fatal(err)
			}
			inputs := map[string]Prepared{"source": {Payload: ref, Filename: "input.bin", Mode: 0o640}}
			_, _, err = prepareResource(t.Context(), store, ops, resourcePlan{Operation: "fixture"}, inputs, t.TempDir(), "")
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "modified immutable input source") {
				t.Fatalf("input %s change: %v", change, err)
			}
		})
	}
}

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
		testproject.Write(t, filename, text)
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
	for _, resource := range first.Resources {
		if artifact := resource.Artifacts["installer"]; artifact.Filename != "branding-1.0.pkg" || artifact.Payload != original {
			t.Fatalf("resource lost the named, unchanged installer: %+v", artifact)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "repo", "pkgs", "branding-1.0.pkg")); err != nil {
		t.Fatalf("published installer name: %v", err)
	}
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

// suspendedProject keeps a private build and the software consuming it out of
// implicit runs while a vendor package stays in them.
const suspendedProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: suspension
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
  name: vendor
spec:
  source:
    path: vendor.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata:
  name: private
suspend: true
spec:
  inputs:
    payload:
      path: private.txt
  payload:
    /Library/Example/private.txt:
      $input: payload
      mode: '0644'
  package:
    identifier: edu.example.private
    version: '1.0'
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
      name: private
      output: installer
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
`

func TestSuspendedResourcesRunOnlyWhenSelected(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private.txt")
	if err := os.WriteFile(private, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	testproject.Write(t, filename, suspendedProject)
	const vendor, build = "stemma/v1alpha1/MacSoftware/vendor", "stemma/v1alpha1/BuildMacPkg/private"
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	keys := func(report Report) []string {
		var keys []string
		for _, resource := range report.Resources {
			keys = append(keys, resource.Key)
		}
		return keys
	}
	locked := func() map[string]map[string]source.Entry {
		t.Helper()
		file, err := lockfile.Load(lockfile.Filename(root))
		if err != nil {
			t.Fatal(err)
		}
		return file.Inputs
	}
	report, err := Run(t.Context(), options)
	if err != nil || !slices.Equal(keys(report), []string{vendor}) {
		t.Fatalf("implicit update touched suspended resources: %v %v", keys(report), err)
	}
	if _, ok := locked()[build]; ok {
		t.Fatal("implicit update acquired a suspended resource")
	}
	options.Resources = []string{"BuildMacPkg/private"}
	if report, err = Run(t.Context(), options); err != nil || !slices.Equal(keys(report), []string{build}) {
		t.Fatalf("selecting a suspended resource did not run it: %v %v", keys(report), err)
	}
	entry := locked()[build]["payload"]
	if entry.Content.Filename != "private.txt" {
		t.Fatalf("selected suspended resource was not locked: %+v", locked())
	}
	// The private input is absent from every fresh checkout.
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	options.Resources = nil
	if report, err = Run(t.Context(), options); err != nil || !slices.Equal(keys(report), []string{vendor}) || !locked()[build]["payload"].Equal(entry) {
		t.Fatalf("implicit update lost the suspended resource's lock: %v %v %+v", keys(report), err, locked())
	}
	options.Method = "apply"
	report, err = Run(t.Context(), options)
	if err != nil || !slices.Equal(keys(report), []string{vendor}) || len(report.Resources[0].Destinations) != 1 || *report.LockChanged {
		t.Fatalf("implicit apply did not skip suspended resources: %v %+v", err, report)
	}
	if err := os.WriteFile(private, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	options.Resources = []string{"MacSoftware/private"}
	report, err = Run(t.Context(), options)
	if err != nil || !slices.Equal(keys(report), []string{build, "stemma/v1alpha1/MacSoftware/private"}) || len(report.Resources[1].Destinations) != 1 {
		t.Fatalf("explicit selection did not run the suspended closure: %v %+v", err, report)
	}
	// A resource the catalog no longer declares still loses its lock implicitly.
	project, resources, _ := strings.Cut(suspendedProject, "\n---\n")
	_, resources, _ = strings.Cut(resources, "\n---\n")
	testproject.Write(t, filename, project+"\n---\n"+resources)
	options.Method, options.Resources = "update", nil
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if inputs := locked(); len(inputs) != 1 || !inputs[build]["payload"].Equal(entry) {
		t.Fatalf("removed resource kept its lock or suspended resource lost it: %+v", inputs)
	}
}

func TestActiveResourceCannotConsumeSuspendedOutputs(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	for name, content := range map[string]string{"vendor.pkg": "", "private.txt": "private"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := strings.Replace(suspendedProject, "suspend: true\nspec:\n  source:", "spec:\n  source:", 1)
	testproject.Write(t, filename, manifest)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update", Resources: []string{"MacSoftware/private"}}
	const want = "resource stemma/v1alpha1/MacSoftware/private input source: depends on suspended resource stemma/v1alpha1/BuildMacPkg/private; suspend stemma/v1alpha1/MacSoftware/private as well"
	if _, err := ValidateProject(t.Context(), options, false); err == nil || err.Error() != want {
		t.Fatalf("validation accepted an active consumer of a suspended build: %v", err)
	}
	if _, err := Run(t.Context(), options); err == nil || err.Error() != want {
		t.Fatalf("explicit selection ran an active consumer of a suspended build: %v", err)
	}
}
