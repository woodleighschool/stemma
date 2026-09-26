package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

const policyProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: policies
spec:
  imports:
    - '*.software.yaml'
  destinations:
    first:
      operation: munki
      config:
        path: first
    second:
      operation: munki
      config:
        path: second
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: policy
spec:
  destinations:
    first:
      pkginfo:
        installer_type: nopkg
        version: '1'
        description: original
        installcheck_script: |
          #!/bin/sh
          exit 1
    second:
      pkginfo:
        installer_type: nopkg
        version: '1'
        installcheck_script: |
          #!/bin/sh
          exit 1
`

func TestSourceFreePublicationAndIndependentFailures(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, policyProject)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply"}
	report, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resources) != 1 || len(report.Resources[0].Artifacts) != 0 || len(report.Resources[0].Destinations) != 2 {
		t.Fatalf("incomplete sourcefree result: %+v", report)
	}
	options.Method = "plan"
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".stemma", "state")); !os.IsNotExist(err) {
		t.Fatal("publication kept local destination state")
	}
	testproject.Write(t, filename, strings.Replace(policyProject, "description: original", "description: edited", 1))
	options.Method = "apply"
	report, err = Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Resources[0].Cached {
		t.Fatal("metadata invalidated sourcefree preparation")
	}
	applied := []string{}
	options.Handlers = map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		if request.Method == "plan" && request.Identity.Destination == "first" {
			return plugin.ReconcileResponse{}, errors.New("unavailable")
		}
		if request.Method == "apply" {
			applied = append(applied, request.Identity.Destination)
		}
		return plugin.ReconcileResponse{}, nil
	}}
	var completed []ResourceReport
	options.ResourceDone = func(result ResourceReport) error {
		if len(result.Destinations) != 2 || len(applied) != 1 {
			t.Fatalf("resource completed before independent destinations: %+v", result)
		}
		completed = append(completed, result)
		return nil
	}
	var logs bytes.Buffer
	ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
	report, err = Run(ctx, options)
	if err == nil || len(applied) != 1 || applied[0] != "second" || !report.Resources[0].Destinations[1].Applied {
		t.Fatalf("independent destination was blocked: %+v %v", report, err)
	}
	if !reflect.DeepEqual(completed, report.Resources) {
		t.Fatalf("streamed results differ from final report: %+v", completed)
	}
	failed := false
	for line := range bytes.SplitSeq(bytes.TrimSpace(logs.Bytes()), []byte{'\n'}) {
		var record struct {
			Message     string `json:"msg"`
			Destination string `json:"destination"`
			Error       string `json:"error"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Destination == "first" {
			failed = failed || record.Error != ""
			if record.Message == "Destination planned" || record.Message == "Destination applied" {
				t.Fatalf("failed destination logged success: %s", line)
			}
		}
	}
	if !failed {
		t.Fatal("failed destination was not logged")
	}
}

func TestPartialFailureReportsCompletedChangesWithoutSuccess(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, policyProject)
	failure := errors.New("upload failed")
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply", Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		if request.Method == "apply" {
			return plugin.ReconcileResponse{Changes: []plugin.Change{{Kind: "metadata", Field: "description", Action: "set"}}}, failure
		}
		return plugin.ReconcileResponse{}, nil
	}}}
	report, err := Run(t.Context(), options)
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for _, destination := range report.Resources[0].Destinations {
		if destination.Applied || len(destination.Changes) != 1 || destination.Error == "" {
			t.Fatalf("failed apply lost its completed changes or reported success: %+v", destination)
		}
	}
}

func TestNativeValidationBeforeAcquisition(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	manifest := strings.Replace(policyProject, "  destinations:\n    first:\n      pkginfo:", "  source:\n    path: missing.pkg\n  destinations:\n    first:\n      pkginfo:", 1)
	manifest = strings.Replace(manifest, "description: original", "unattended_install: invalid", 1)
	testproject.Write(t, filename, manifest)
	_, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply"})
	if err == nil || !strings.Contains(err.Error(), "unattended_install") {
		t.Fatalf("invalid native field reached acquisition: %v", err)
	}
}

// TestOpenReleasesAPartialSessionOnFailure covers a project whose lockfile
// cannot be read while its plugins load: opening fails before any operation
// runs and releases what it had acquired instead of panicking.
func TestOpenReleasesAPartialSessionOnFailure(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	manifest := strings.Replace(policyProject, "  imports:\n", "  plugins:\n    missing:\n      path: plugins/missing\n      trusted: true\n  imports:\n", 1)
	testproject.Write(t, filename, manifest)
	if err := os.WriteFile(filepath.Join(root, "stemma.lock.yaml"), []byte("version: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply"})
	if err == nil || !strings.Contains(err.Error(), "lockfile") {
		t.Fatalf("unreadable lockfile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".stemma", "project.lock")); err != nil {
		t.Fatalf("project lock was not created: %v", err)
	}
	if unlock, err := lockfile.Lock(t.Context(), root); err != nil {
		t.Fatalf("project lock still held after the failed open: %v", err)
	} else {
		_ = unlock()
	}
}

func TestOpenEvidencePreservesNativeTypes(t *testing.T) {
	effective, _, err := resolveMetadata(plugin.ResourceResult{}, map[string]any{"count": "{{ evidence['vendor.probe'].count }}", "enabled": "{{ evidence['vendor.probe'].enabled }}"}, plugin.Facts{}, map[string]json.RawMessage{"vendor.probe": json.RawMessage(`{"count":4,"enabled":false}`)})
	if err != nil || effective["count"] != float64(4) || effective["enabled"] != false {
		t.Fatalf("evidence handoff: %+v %v", effective, err)
	}
	if _, _, err := resolveMetadata(plugin.ResourceResult{}, map[string]any{"value": "{{ evidence['vendor.missing'].value }}"}, plugin.Facts{}, nil); err == nil {
		t.Fatal("missing evidence accepted")
	}
}

func TestSourceFreeCannotSilentlySkipVerification(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	manifest := strings.Replace(policyProject, "spec:\n  destinations:\n    first:\n      pkginfo:", "spec:\n  signature:\n    signer: apple:developer-id:SMLKBTR495\n  destinations:\n    first:\n      pkginfo:", 1)
	testproject.Write(t, filename, manifest)
	if _, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply"}); err == nil || !strings.Contains(err.Error(), "require a source") {
		t.Fatalf("sourcefree verification was skipped: %v", err)
	}
}

func TestResourceWorkStaysInItsResourceTree(t *testing.T) {
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(installer) }))
	defer server.Close()
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	testproject.Write(t, filename, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: scopes}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: app}
spec:
  source: {url: %s/app.pkg}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`, server.URL))
	cache := t.TempDir()
	for _, method := range []string{"update", "prepare", "plan"} {
		var logs bytes.Buffer
		ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
		if _, err := Run(ctx, Options{ConfigPath: filename, CacheDir: cache, Method: method}); err != nil {
			t.Fatal(err)
		}
		// Terminal progress groups stages by resource; a project stage open
		// around resource work would show one operation in two trees.
		var project []string
		acquired := false
		for line := range bytes.SplitSeq(bytes.TrimSpace(logs.Bytes()), []byte{'\n'}) {
			var record struct {
				Message  string `json:"msg"`
				Resource string `json:"resource"`
				Start    bool   `json:"stage"`
				End      bool   `json:"stage_result"`
			}
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			if bytes.Count(line, []byte(`"resource":`)) > 1 {
				t.Fatalf("%s repeated the resource scope: %s", method, line)
			}
			switch {
			case record.Resource == "" && record.Start:
				project = append(project, record.Message)
			case record.Resource == "" && record.End:
				project = slices.DeleteFunc(project, func(message string) bool { return message == record.Message })
			case record.Start && len(project) > 0:
				t.Fatalf("%s started %s inside project stages %v", method, record.Message, project)
			}
			acquired = acquired || record.Start && record.Message == "Acquiring input" && record.Resource == "MacSoftware/app"
		}
		if !acquired {
			t.Fatalf("%s did not acquire within the resource tree: %s", method, logs.String())
		}
	}
}

func TestPrepareFinishesEachResourceBeforeAcquiringTheNext(t *testing.T) {
	for _, cancelDuringValidation := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelDuringValidation), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			var events []string
			record := func(event string) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, event)
			}
			// Distinct packages, since preparation fetches content it does not hold.
			packages := map[string][]byte{"/a.pkg": testPackage(t, "com.example.a"), "/b.pkg": testPackage(t, "com.example.b")}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record("acquire " + r.URL.Path)
				_, _ = w.Write(packages[r.URL.Path])
			}))
			defer server.Close()
			root := t.TempDir()
			manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: sequential}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
`
			for _, name := range []string{"a", "b"} {
				manifest += fmt.Sprintf(`---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: %s}
spec:
  source: {url: %s/%s.pkg}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`, name, server.URL, name)
			}
			filename := filepath.Join(root, "stemma.yaml")
			testproject.Write(t, filename, manifest)
			// Update locks the inputs into another cache, so preparation acquires them.
			if _, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}); err != nil {
				t.Fatal(err)
			}
			locked, err := os.ReadFile(lockfile.Filename(root))
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			events = nil
			mu.Unlock()
			var logs bytes.Buffer
			ctx = plugin.WithLogger(ctx, slog.New(slog.NewJSONHandler(&logs, nil)))
			report, err := Run(ctx, Options{
				ConfigPath: filename, CacheDir: t.TempDir(), Method: "prepare",
				Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
					if request.Prepared {
						record("validate " + request.Identity.Resource.Name)
						if cancelDuringValidation {
							cancel()
							return plugin.ReconcileResponse{}, ctx.Err()
						}
					}
					return plugin.ReconcileResponse{}, nil
				}},
				ResourceDone: func(result ResourceReport) error { record("done " + result.Name); return nil },
			})
			mu.Lock()
			defer mu.Unlock()
			if cancelDuringValidation {
				if !errors.Is(err, context.Canceled) || len(report.Resources) != 1 || report.LockChanged != nil {
					t.Fatalf("cancellation did not stop the run: %+v %v", report, err)
				}
				if got := strings.Join(events, ", "); got != "acquire /a.pkg, validate a" {
					t.Fatalf("work continued after cancellation: %s", got)
				}
				if strings.Contains(logs.String(), "Preparation failed") || strings.Contains(logs.String(), "Destination failed") {
					t.Fatalf("cancellation logged ordinary failures: %s", logs.String())
				}
				if after, err := os.ReadFile(lockfile.Filename(root)); err != nil || !bytes.Equal(after, locked) {
					t.Fatalf("cancelled run changed the lockfile: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Join(events, ", "); got != "acquire /a.pkg, validate a, done a, acquire /b.pkg, validate b, done b" {
					t.Fatalf("resource work was split into passes: %s", got)
				}
			}
		})
	}
}

func TestApplyKeepsCrossedPublicationDependenciesIndependent(t *testing.T) {
	parts := strings.Split(policyProject, "\n---\n")
	manifest := parts[0]
	for _, name := range []string{"a", "b"} {
		resource := strings.Replace(parts[1], "name: policy", "name: "+name, 1)
		if name == "a" {
			resource = strings.Replace(resource, "description: original", "requires: [{resource: {kind: MacSoftware, name: b}}]", 1)
		}
		if name == "b" {
			at := strings.Index(resource, "    second:")
			resource = resource[:at] + strings.Replace(resource[at:], "pkginfo:", "pkginfo:\n        requires: [{resource: {kind: MacSoftware, name: a}}]", 1)
		}
		manifest += "\n---\n" + resource
	}
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	testproject.Write(t, filename, manifest)
	var applied []string
	report, err := Run(t.Context(), Options{
		ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply",
		Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
			identity := request.Identity.Resource.Name + "/" + request.Identity.Destination

			if request.Method == "apply" {
				applied = append(applied, identity)
			}
			return plugin.ReconcileResponse{}, nil
		}},
	})
	if err != nil || len(report.Resources) != 2 {
		t.Fatalf("crossed destinations blocked execution: %+v %v", report, err)
	}
	if got := strings.Join(applied, ", "); got != "b/first, a/first, a/second, b/second" {
		t.Fatalf("publication dependencies lost: %s", got)
	}
}

func TestApplyOrdersRequiredResourcesAndLeavesOthersToDestinations(t *testing.T) {
	parts := strings.Split(policyProject, "\n---\n")
	manifest := parts[0]
	for _, name := range []string{"agent", "suite"} {
		resource := strings.Replace(parts[1], "name: policy", "name: "+name, 1)
		if name == "agent" {
			resource = strings.ReplaceAll(resource, "pkginfo:", "pkginfo:\n        requires: [{resource: {kind: MacSoftware, name: suite}}, GarageBand]")
		}
		manifest += "\n---\n" + resource
	}
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	testproject.Write(t, filename, manifest)
	var applied []string
	report, err := Run(t.Context(), Options{
		ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply",
		Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
			identity := request.Identity.Resource.Name + "/" + request.Identity.Destination

			if request.Method == "apply" {
				applied = append(applied, identity)
			}
			return plugin.ReconcileResponse{}, nil
		}},
	})
	if err != nil || len(report.Resources) != 2 {
		t.Fatalf("external reference blocked execution: %+v %v", report, err)
	}
	if got := strings.Join(applied, ", "); got != "suite/first, agent/first, suite/second, agent/second" {
		t.Fatalf("required resource not reconciled first: %s", got)
	}
}

func TestApplyDoesNotSelectPublicationPeers(t *testing.T) {
	parts := strings.Split(policyProject, "\n---\n")
	consumer := strings.Replace(parts[1], "name: policy", "name: consumer", 1)
	consumer = strings.ReplaceAll(consumer, "pkginfo:", "pkginfo:\n        requires: [{resource: {kind: MacSoftware, name: peer}}]")
	peer := strings.Replace(parts[1], "name: policy", "name: peer", 1)
	peer = strings.ReplaceAll(peer, "pkginfo:", "pkginfo:\n        name: Native Peer")
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	testproject.Write(t, filename, parts[0]+"\n---\n"+consumer+"\n---\n"+peer)
	var applied []string
	report, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply", Resources: []string{"consumer"}, Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		if request.Identity.Resource.Name != "consumer" {
			t.Fatalf("unselected peer invoked: %+v", request.Identity)
		}
		if request.Method == "apply" {
			var metadata struct {
				Pkginfo struct {
					Name string `json:"name"`
				} `json:"pkginfo"`
			}
			if err := json.Unmarshal(request.Peers["stemma/v1alpha1/MacSoftware/peer"], &metadata); err != nil || metadata.Pkginfo.Name != "Native Peer" {
				t.Fatalf("peer metadata lost: %+v, %v", metadata, err)
			}
			applied = append(applied, request.Identity.Destination)
		}
		return plugin.ReconcileResponse{}, nil
	}}})
	if err != nil || len(report.Resources) != 1 || !slices.Equal(applied, []string{"first", "second"}) {
		t.Fatalf("unexpected selected publications: %+v, %v, %v", report, applied, err)
	}
}

func TestApplyChecksEachResourcesReviewedInputsBeforeWriting(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	parts := strings.Split(policyProject, "\n---\n")
	manifest := parts[0]
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, name+".pkg"), installer, 0o644); err != nil {
			t.Fatal(err)
		}
		resource := strings.Replace(parts[1], "name: policy", "name: "+name, 1)
		resource = strings.Replace(resource, "spec:\n", "spec:\n  source: {path: "+name+".pkg}\n", 1)
		manifest += "\n---\n" + resource
	}
	testproject.Write(t, filename, manifest)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.pkg"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	writes := 0
	options.Method = "apply"
	options.Handlers = map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		if request.Method == "apply" {
			if request.Identity.Resource.Name != "a" {
				t.Fatalf("published unreviewed input: %+v", request.Identity)
			}
			writes++
		}
		return plugin.ReconcileResponse{}, nil
	}}
	if report, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "local input content changed") || writes != 2 || len(report.Resources) != 2 {
		t.Fatalf("apply did not isolate the stale resource: writes=%d report=%+v error=%v", writes, report, err)
	}
}

// TestReleaseArchiveApplicationPublishesADiskImage follows a GitHub release: one
// MacSoftware acquires the ZIP, verifies the bundle's signer and publishes a disk
// image that Munki installs without declared copy or detection fields.
func TestReleaseArchiveApplicationPublishesADiskImage(t *testing.T) {
	release := t.TempDir()
	if err := os.CopyFS(filepath.Join(release, "WoodSweep.app"), os.DirFS("../apple/testdata/SignedFixture.app")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "WoodSweep-1.2.3.zip")
	testarchive.Zip(t, archive, release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, archive) }))
	defer server.Close()
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: release}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: woodsweep}
spec:
  source: {url: %s/WoodSweep-1.2.3.zip}
  signature: {signer: 'apple:developer-id:SMLKBTR495'}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`, server.URL))
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "apply"
	report, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	installer := report.Resources[0].Artifacts["installer"]
	if installer.Format != "dmg" || installer.Filename != "woodsweep-1.2.3.dmg" || installer.Evidence["signature"] == nil {
		t.Fatalf("installer = %+v", installer)
	}
	pkginfos, err := filepath.Glob(filepath.Join(root, "repo/pkgsinfo/*.plist"))
	if err != nil || len(pkginfos) != 1 {
		t.Fatalf("pkginfo files %v: %v", pkginfos, err)
	}
	data, err := os.ReadFile(pkginfos[0])
	if err != nil {
		t.Fatal(err)
	}
	var pkginfo struct {
		InstallerType string `plist:"installer_type"`
		Location      string `plist:"installer_item_location"`
		Version       string `plist:"version"`
		ItemsToCopy   []struct {
			SourceItem      string `plist:"source_item"`
			DestinationPath string `plist:"destination_path"`
		} `plist:"items_to_copy"`
		Installs []struct {
			Path     string `plist:"path"`
			BundleID string `plist:"CFBundleIdentifier"`
		} `plist:"installs"`
	}
	if _, err := plist.Unmarshal(data, &pkginfo); err != nil {
		t.Fatal(err)
	}
	if pkginfo.InstallerType != "copy_from_dmg" || pkginfo.Version != "1.2.3" || len(pkginfo.ItemsToCopy) != 1 || pkginfo.ItemsToCopy[0].SourceItem != "WoodSweep.app" || pkginfo.ItemsToCopy[0].DestinationPath != "/Applications" || len(pkginfo.Installs) != 1 || pkginfo.Installs[0].Path != "/Applications/WoodSweep.app" || pkginfo.Installs[0].BundleID != "au.edu.vic.woodleigh.stemma.fixture" {
		t.Fatalf("pkginfo = %+v", pkginfo)
	}
	published, err := diskimage.Open(t.Context(), filepath.Join(root, "repo/pkgs", filepath.FromSlash(pkginfo.Location)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = published.Close() }()
	if info, err := fs.Stat(published, "WoodSweep.app"); err != nil || !info.IsDir() {
		t.Fatalf("published image lacks the application: %v", err)
	}
}
