package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestPreparationTreeLeaseContract(t *testing.T) {
	for _, encoded := range []bool{false, true} {
		for _, change := range []string{"none", "bytes", "member mode", "link target"} {
			t.Run(fmt.Sprintf("tar=%v/%s", encoded, change), func(t *testing.T) {
				if !encoded && change != "none" {
					t.Skip("directory mutation covered by lease digest tests")
				}
				store, err := cas.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				directory := t.TempDir()
				if err := os.WriteFile(filepath.Join(directory, "data"), []byte("original"), 0o640); err != nil {
					t.Fatal(err)
				}
				ref, err := importPath(t.Context(), store, directory, true)
				if err != nil {
					t.Fatal(err)
				}
				ops := &operations{registry: plugin.New("fixture", "1")}
				operation := resourceOperation("fixture", "Fixture")
				operation.TarInputs = encoded
				operation.InputSchema, operation.OutputSchema = json.RawMessage(`{}`), json.RawMessage(`{}`)
				err = ops.registry.Register(operation, func(_ context.Context, envelope plugin.Request) (plugin.Response, error) {
					var request plugin.ResourceRequest[json.RawMessage]
					if err := json.Unmarshal(envelope.Input, &request); err != nil {
						return plugin.Response{}, err
					}
					input := request.Inputs["source"]
					info, err := os.Stat(input.Path)
					if err != nil {
						return plugin.Response{}, err
					}
					if !input.Tree || input.Filename != "Input.app" || input.Mode != 0o750 || info.IsDir() == encoded || (input.Encoding == "tar") != encoded {
						t.Fatalf("wrong lease: %+v, %v", input, info)
					}
					if change != "none" {
						var data bytes.Buffer
						w := tar.NewWriter(&data)
						header := &tar.Header{Name: "data", Typeflag: tar.TypeReg, Mode: 0o640, Size: 8}
						content := "original"
						switch change {
						case "bytes":
							content = "modified"
						case "member mode":
							header.Mode = 0o600
						case "link target":
							header.Typeflag, header.Linkname, header.Size, content = tar.TypeSymlink, "other", 0, ""
						}
						if err := w.WriteHeader(header); err != nil {
							return plugin.Response{}, err
						}
						if _, err := w.Write([]byte(content)); err != nil {
							return plugin.Response{}, err
						}
						if err := w.Close(); err != nil {
							return plugin.Response{}, err
						}
						if err := os.WriteFile(input.Path, data.Bytes(), 0o600); err != nil {
							return plugin.Response{}, err
						}
					}
					return plugin.Response{Output: json.RawMessage(`{}`)}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				inputs := map[string]Prepared{"source": {Payload: ref, Filename: "Input.app", Tree: true, Mode: 0o750}}
				_, _, err = prepareResource(t.Context(), store, ops, resourcePlan{Operation: "fixture"}, inputs, t.TempDir(), "")
				if change == "none" && err != nil {
					t.Fatal(err)
				}
				if change != "none" && (err == nil || !strings.Contains(err.Error(), "modified immutable input source")) {
					t.Fatalf("lease mutation accepted: %v", err)
				}
				if err := store.Verify(t.Context(), ref); err != nil {
					t.Fatalf("lease shares cache storage: %v", err)
				}
			})
		}
	}
}

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
			operation := plugin.Operation{Name: "fixture", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "example.test/v1", Kind: "Fixture"}, SideEffects: "workspace", Methods: []string{"discover", "run"}, InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`)}
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

func TestBuiltPackageNeedsNoSigningExpectation(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: built}
spec:
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata: {name: tool}
spec:
  payload:
    /Library/Example/tool.txt: {content: tool}
  package: {identifier: com.example.tool, version: '1.0'}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: tool}
spec:
  source: {resource: {kind: BuildMacPkg, name: tool}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: vendor}
spec:
  source: {path: vendor.pkg}
`
	testproject.Write(t, filename, manifest)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "signature"
	derived, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	// Only the vendor package has a publisher to declare.
	var proposed []string
	for _, resource := range derived.Resources {
		if resource.Artifacts["installer"].Evidence["signatures"] != nil {
			proposed = append(proposed, resource.Kind+"/"+resource.Name)
		}
	}
	if !slices.Equal(proposed, []string{"MacSoftware/vendor"}) || derived.Summary.Derived != 1 {
		t.Fatalf("derived expectations for %v, counted %d", proposed, derived.Summary.Derived)
	}
	// An expectation declared for the built package is still held to it.
	options.Method = "prepare"
	for declared, message := range map[string]string{"unsigned: true": "", "signer: apple:developer-id:SMLKBTR495": "not signed"} {
		testproject.Write(t, filename, strings.Replace(manifest, "name: tool}}\n", "name: tool}}\n  signatures: [{"+declared+"}]\n", 1))
		report, _ := Run(t.Context(), options)
		index := slices.IndexFunc(report.Resources, func(resource ResourceReport) bool {
			return resource.Kind == "MacSoftware" && resource.Name == "tool"
		})
		if index < 0 {
			t.Fatalf("%s: software was not prepared: %+v", declared, report.Resources)
		}
		resource := report.Resources[index]
		verified := resource.Artifacts["installer"].Evidence["signatures"] != nil
		if message == "" && !verified || message != "" && !strings.Contains(resource.Error, message) {
			t.Fatalf("%s: verified %t, error %q", declared, verified, resource.Error)
		}
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
	if err != nil || !slices.Equal(keys(report), []string{vendor}) || len(report.Resources[0].Destinations) != 1 || report.LockChanged != nil {
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

// profiledProject keeps software captured on one prepared machine out of runs
// that select no profile. Paused is suspended inside the same profile.
const profiledProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: profiles
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
kind: MacSoftware
metadata:
  name: captured
profiles: [capture]
spec:
  source:
    path: captured.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: paused
profiles: [capture]
suspend: true
spec:
  source:
    path: paused.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
`

func TestProfiledResourcesRunOnlyWhenSelected(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	testproject.Write(t, filename, profiledProject)
	const vendor, captured = "stemma/v1alpha1/MacSoftware/vendor", "stemma/v1alpha1/MacSoftware/captured"
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
	// The captured package exists only on the machine that runs the profile.
	report, err := Run(t.Context(), options)
	if err != nil || !slices.Equal(keys(report), []string{vendor}) || len(locked()) != 1 {
		t.Fatalf("update without a profile touched profiled resources: %v %v %+v", keys(report), err, locked())
	}
	capture := filepath.Join(root, "captured.pkg")
	if err := os.WriteFile(capture, testPackage(t, "com.example.captured"), 0o644); err != nil {
		t.Fatal(err)
	}
	options.Profiles = []string{"capture"}
	if report, err = Run(t.Context(), options); err != nil || !slices.Equal(keys(report), []string{captured}) {
		t.Fatalf("profile update did not run exactly its unsuspended resources: %v %v", keys(report), err)
	}
	entries := locked()
	if len(entries) != 2 || entries[captured]["source"].Content.Filename != "captured.pkg" || len(entries[vendor]) != 1 {
		t.Fatalf("profile update did not lock its resources beside the others: %+v", entries)
	}
	options.Method = "apply"
	report, err = Run(t.Context(), options)
	if err != nil || !slices.Equal(keys(report), []string{captured}) || len(report.Resources[0].Destinations) != 1 {
		t.Fatalf("profile apply did not publish exactly its resources: %v %+v", err, report)
	}
	options.Profiles, options.Resources = nil, []string{"MacSoftware/captured"}
	if report, err = Run(t.Context(), options); err != nil || !slices.Equal(keys(report), []string{captured}) {
		t.Fatalf("naming a profiled resource did not run it: %v %v", keys(report), err)
	}
	options.Profiles = []string{"capture"}
	if _, err = Run(t.Context(), options); err == nil || err.Error() != "select resources by profile or by name, not both" {
		t.Fatalf("a run took a profile and a selector: %v", err)
	}
	if err := os.Remove(capture); err != nil {
		t.Fatal(err)
	}
	options.Profiles, options.Resources = nil, nil
	report, err = Run(t.Context(), options)
	if err != nil || !slices.Equal(keys(report), []string{vendor}) || len(report.Resources[0].Destinations) != 1 {
		t.Fatalf("apply without a profile did not skip profiled resources: %v %+v", err, report)
	}
	options.Method = "update"
	if report, err = Run(t.Context(), options); err != nil || !slices.Equal(keys(report), []string{vendor}) || !locked()[captured]["source"].Equal(entries[captured]["source"]) {
		t.Fatalf("update without a profile lost the profiled resource's lock: %v %v %+v", keys(report), err, locked())
	}
}
