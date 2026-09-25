package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestArtifactMaterializesAReviewedOutputWithoutDestinations(t *testing.T) {
	root := t.TempDir()
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: artifacts}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: app}
spec:
  source: {path: app.pkg}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`)
	cache := t.TempDir()
	options := Options{ConfigPath: filename, CacheDir: cache, Method: "artifact", Resources: []string{"MacSoftware/app"}, Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		if request.Method != "validate" || request.Prepared {
			t.Errorf("artifact reached destination %s with a prepared artifact", request.Method)
		}
		return plugin.ReconcileResponse{}, nil
	}}}
	lockPath := filepath.Join(root, "stemma.lock.yaml")
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "run stemma update") {
		t.Fatalf("artifact prepared an unreviewed input: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("artifact wrote the lockfile")
	}
	update := options
	update.Method, update.Resources, update.Handlers = "update", nil, nil
	if _, err := Run(t.Context(), update); err != nil {
		t.Fatal(err)
	}
	reviewed, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		report, err := Run(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		prepared := report.Resources[0].Artifacts["installer"]
		if want := filepath.Join(cache, "materialized", prepared.Payload.SHA256, prepared.Filename); report.Artifact != want {
			t.Fatalf("artifact = %s, want %s", report.Artifact, want)
		}
		data, err := os.ReadFile(report.Artifact)
		if err != nil {
			t.Fatal(err)
		}
		if digest := sha256.Sum256(data); hex.EncodeToString(digest[:]) != prepared.Payload.SHA256 {
			t.Fatalf("attempt %d materialized other bytes", attempt)
		}
		// The next run replaces a copy changed after materialization.
		if err := os.Chmod(report.Artifact, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(report.Artifact, []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if current, err := os.ReadFile(lockPath); err != nil || !bytes.Equal(current, reviewed) {
		t.Fatalf("artifact changed the lockfile: %v", err)
	}
	options.Output = "uninstaller"
	if report, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "no uninstaller output; the resource prepares installer") || report.Artifact != "" {
		t.Fatalf("missing output: %q %v", report.Artifact, err)
	}
}

// TestArtifactWithoutInputLocksPreparesTheSourceAsItIs changes a reviewed
// application: the locked artifact refuses it, and --no-input-lock prepares it
// without changing the lockfile.
func TestArtifactWithoutInputLocksPreparesTheSourceAsItIs(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "Fixture.app")
	if err := os.CopyFS(app, os.DirFS("../apple/testdata/Fixture.app")); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: artifacts}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: app}
spec:
  source: {path: Fixture.app}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "stemma.lock.yaml")
	reviewed, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(app, "Contents", "Info.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, bytes.Replace(data, []byte("<string>1.2.3</string>"), []byte("<string>2.0.0</string>"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	options.Method, options.Resources = "artifact", []string{"MacSoftware/app"}
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "local input content changed; run stemma update") {
		t.Fatalf("locked artifact accepted a changed input: %v", err)
	}
	options.Lock.IgnoreInputs = true
	report, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if installer := report.Resources[0].Artifacts["installer"]; installer.Version != "2.0.0" || filepath.Base(report.Artifact) != installer.Filename {
		t.Fatalf("artifact %s for %+v", report.Artifact, installer)
	}
	if current, err := os.ReadFile(lockPath); err != nil || !bytes.Equal(current, reviewed) {
		t.Fatalf("--no-input-lock changed the lockfile: %v", err)
	}
}

// TestArtifactWithoutInputLocksKeepsPluginsLocked declares a local plugin the
// lockfile has not reviewed: ignoring input locks must not run it.
func TestArtifactWithoutInputLocksKeepsPluginsLocked(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, strings.Replace(policyProject, "  imports:\n", "  plugins:\n    probe:\n      path: plugins/probe\n      trusted: true\n  imports:\n", 1))
	if err := os.MkdirAll(filepath.Join(root, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "probe"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "artifact", Resources: []string{"MacSoftware/policy"}, Lock: lockfile.Options{IgnoreInputs: true}}
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "run stemma plugins update") {
		t.Fatalf("an unreviewed plugin loaded: %v", err)
	}
}

func TestExposeReplacesCopiesFromTheVerifiedObject(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(t.TempDir(), "Fixture.app")
	if err := os.CopyFS(app, os.DirFS("../apple/testdata/SignedFixture.app")); err != nil {
		t.Fatal(err)
	}
	tree, err := importPath(t.Context(), store, app, true, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Import(t.Context(), strings.NewReader("installer"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, prepared := range []Prepared{
		{Payload: tree, Filename: "Fixture.app", Tree: true, Mode: 0o755},
		{Payload: file, Filename: "setup.exe", Mode: 0o755},
	} {
		t.Run(prepared.Filename, func(t *testing.T) {
			var path string
			for range 2 {
				path, err = expose(t.Context(), store, prepared, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if copied, err := importPath(t.Context(), store, path, prepared.Tree, t.TempDir()); err != nil || copied != prepared.Payload {
					t.Fatalf("copy %+v differs from %+v: %v", copied, prepared.Payload, err)
				}
				changed := path
				if prepared.Tree {
					changed = filepath.Join(path, "Contents", "Info.plist")
				}
				if err := os.WriteFile(changed, []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if path != filepath.Join(store.Dir, "materialized", prepared.Payload.SHA256, prepared.Filename) {
				t.Fatalf("path = %s", path)
			}
		})
	}
	object, err := store.Path(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(object, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte("corrupt!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := expose(t.Context(), store, Prepared{Payload: file, Filename: "setup.exe", Mode: 0o755}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("a corrupt object was materialized: %v", err)
	}
}
