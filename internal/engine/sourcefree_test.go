package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testproject"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

func TestSourceFreeMunkiColdFrozenAndMetadataChanges(t *testing.T) {
	for _, rendered := range []bool{false, true} {
		name := "native"
		if rendered {
			name = "rendered"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: source-free}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, path: repo}
---
apiVersion: stemma/v1alpha1
kind: Software
metadata: {name: fixture}
spec:
  destinations:
    repo:
      name: NativeName
      version: "1.0"
      installer_type: nopkg
      description: original
      installcheck_script: "#!/bin/sh\nexit 1\n"
      postinstall_script: "#!/bin/sh\nexit 0\n"
`
			if rendered {
				manifest = strings.Replace(manifest, "spec:\n  destinations:", `spec:
  steps:
    - name: pkginfo
      operation: munki.pkginfo
      config:
        name: NativeName
        version: "1.0"
        installer_type: nopkg
        installcheck_script: "#!/bin/sh\nexit 1\n"
        postinstall_script: "#!/bin/sh\nexit 0\n"
  destinations:`, 1)
				manifest = strings.Replace(manifest, `      name: NativeName
      version: "1.0"
      installer_type: nopkg
      description: original
      installcheck_script: "#!/bin/sh\nexit 1\n"
      postinstall_script: "#!/bin/sh\nexit 0\n"`, "      artifact: pkginfo/artifact\n      description: original", 1)
			}
			path := filepath.Join(root, "stemma.yaml")
			write := func(data string) {
				t.Helper()
				if err := testproject.Write(path, []byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			write(manifest)
			opts := Options{ConfigPath: path, CacheDir: t.TempDir(), Method: "apply", Lock: lockfile.Options{Frozen: true, Offline: true}}
			run := func() SoftwareReport {
				t.Helper()
				report, err := Run(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if report.LockChanged {
					t.Fatal("source-free run changed a source lock")
				}
				item := report.Software[0]
				if item.Prepared != nil || len(item.Artifacts) != 0 || item.SourceCached || len(item.Destinations) != 1 || item.Destinations[0].SourceChanged {
					t.Fatalf("source-free run invented acquisition: %+v", item)
				}
				if !rendered && item.Destinations[0].PreparedChanged {
					t.Fatalf("source-free native destination invented a payload: %+v", item)
				}
				for _, change := range item.Destinations[0].Changes {
					if change.Kind == "content" {
						t.Fatalf("nopkg attempted installer publication: %+v", change)
					}
				}
				return item
			}
			first := run()
			encoded, err := json.Marshal(first)
			if err != nil || strings.Contains(string(encoded), `"source":`) {
				t.Fatalf("source-free report invented source provenance: %s: %v", encoded, err)
			}
			lockPath := filepath.Join(root, "stemma.lock.yaml")
			if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source-free run created a lockfile: %v", err)
			}
			if warm := run(); len(warm.Destinations[0].Changes) != 0 || rendered && !warm.Steps[0].Cached {
				t.Fatalf("warm run was not idempotent: %+v", warm)
			}
			write(strings.Replace(manifest, "description: original", "description: edited", 1))
			changed := run()
			if len(changed.Destinations[0].Changes) == 0 {
				t.Fatal("metadata edit was not reconciled")
			}
			for _, change := range changed.Destinations[0].Changes {
				if change.Field != "description" && !strings.HasPrefix(change.Field, "catalogs/") {
					t.Fatalf("metadata change affected other state: %+v", change)
				}
			}
			opts.CacheDir = t.TempDir()
			cold := run()
			if len(cold.Destinations[0].Changes) != 0 || rendered && (cold.Steps[0].Cached || cold.Steps[0].Artifacts["artifact"].Payload != first.Steps[0].Artifacts["artifact"].Payload) {
				t.Fatalf("cold frozen run changed deterministic output: %+v", cold)
			}
			if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("metadata/cache changes created a source lock: %v", err)
			}
			files, err := filepath.Glob(filepath.Join(root, "repo", "pkgsinfo", "stemma", "*.plist"))
			if err != nil || len(files) != 1 {
				t.Fatalf("pkginfo binding was duplicated: %v: %v", files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if _, err := plist.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			if document["name"] != "NativeName" || document["version"] != "1.0" || document["installer_type"] != "nopkg" || document["postinstall_script"] != "#!/bin/sh\nexit 0\n" {
				t.Fatalf("native nopkg fields changed: %+v", document)
			}
			for _, field := range []string{"installer_item_hash", "installer_item_size", "installer_item_location"} {
				if _, exists := document[field]; exists {
					t.Fatalf("nopkg contains %s", field)
				}
			}
		})
	}
}

func TestMunkiPkginfoRejectsMissingAndUnexpectedInstaller(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
		inputs map[string]plugin.Artifact
	}{
		{"missing-installer", `{"name":"fixture","version":"1","installer_type":"pkg"}`, nil},
		{"unexpected-installer", `{"name":"fixture","version":"1","installer_type":"nopkg"}`, map[string]plugin.Artifact{"input": {Path: "unopened.pkg"}}},
		{"missing-version", `{"name":"fixture","installer_type":"nopkg"}`, nil},
		{"missing-name", `{"version":"1","installer_type":"nopkg"}`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(plugin.StepRequest{Config: json.RawMessage(tt.config), Inputs: tt.inputs})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := munkiOperation(t.Context(), plugin.Request{Method: "validate", Input: data}); err == nil {
				t.Fatal("invalid installer contract accepted")
			}
		})
	}
}

func TestSourceFreeVerificationStillRequiresSubject(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stemma.yaml")
	if err := testproject.Write(path, []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: verification}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: Software
metadata: {name: fixture}
spec: {verification: {integrity: true}}
`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), Options{ConfigPath: path, CacheDir: t.TempDir(), Method: "prepare"}); err == nil || !strings.Contains(err.Error(), "verification subject references missing output prepared") {
		t.Fatalf("configured verification silently skipped absent subject: %v", err)
	}
}

func TestSourceFreePrepareRejectsInstallerDestination(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stemma.yaml")
	if err := testproject.Write(path, []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: missing-installer}
spec:
  imports: ['*.software.yaml']
  destinations: {repo: {operation: munki, path: repo}}
---
apiVersion: stemma/v1alpha1
kind: Software
metadata: {name: fixture}
spec:
  destinations:
    repo: {name: fixture, version: "1", installer_type: pkg}
`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), Options{ConfigPath: path, CacheDir: t.TempDir(), Method: "prepare"}); err == nil {
		t.Fatal("preparation accepted an installer destination without an installer")
	}
	if _, err := os.Stat(filepath.Join(root, "repo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid preparation mutated the destination: %v", err)
	}
}
