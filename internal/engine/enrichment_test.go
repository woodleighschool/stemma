package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/woodleighschool/stemma/internal/testproject"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"howett.net/plist"
)

func TestInspectedAppBuildDrivesMunkiWithoutReplacingInstaller(t *testing.T) {
	root := t.TempDir()
	buildEnrichmentPackage(t, root)
	installer, err := os.ReadFile(filepath.Join(root, "vendor.pkg"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(installer)
	wantDigest := hex.EncodeToString(digest[:])
	manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: enrichment
spec:
  destinations:
    derived: {operation: munki, config: {path: derived}}
    authored: {operation: munki, config: {path: authored}}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: example
spec:
  source: {type: file, path: vendor.pkg}
  subjects:
    main: {bundle_id: org.example.app}
  steps:
    - name: observed
      operation: inspect
      inputs: {input: source}
  destinations:
    derived:
      installer: observed/artifact
      pkginfo:
        version: {$fact: main.app.build}
    authored:
      installer: observed/artifact
      pkginfo:
        version: {$fact: main.app.build}
        receipts: [{packageid: org.example.authored, version: "6"}]
        installs: [{type: file, path: /Library/Example/installed}]
        unattended_install: true
        description: previous
`
	configPath := filepath.Join(root, "stemma.yaml")
	if err := testproject.Write(configPath, []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: configPath, CacheDir: t.TempDir(), Method: "plan"}
	plan, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"derived", "authored"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("planning wrote %s repository: %v", name, err)
		}
	}
	if len(plan.Software) != 1 || len(plan.Software[0].Steps) != 1 {
		t.Fatalf("missing inspection report: %+v", plan)
	}
	observed := plan.Software[0].Steps[0].Artifacts["artifact"]
	if observed.Format != "pkg" || observed.Tree || observed.Payload.SHA256 != wantDigest || observed.Source.Artifact.SHA256 != wantDigest {
		t.Fatalf("inspection replaced the installer: %+v", observed)
	}
	var appSeen, receiptSeen bool
	for _, subject := range observed.Facts.Subjects {
		if subject.App != nil {
			appSeen = true
			if subject.App.Version != "1.2" || subject.App.Build != "100" || subject.Path != "Payload" || subject.InstalledPath != "/Applications/Example.app" {
				t.Fatalf("app versions or path provenance changed: %+v", subject)
			}
		}
		if subject.Package != nil {
			receiptSeen = true
			if subject.Package.Version != "9" {
				t.Fatalf("consumer build overwrote receipt: %+v", subject.Package)
			}
		}
	}
	if !appSeen || !receiptSeen {
		t.Fatalf("missing independent app and receipt facts: %+v", observed.Facts)
	}
	for _, destination := range plan.Software[0].Destinations {
		versionSeen := false
		for _, change := range destination.Changes {
			if change.Field == "version" {
				versionSeen = string(change.After) == `"100"`
			}
		}
		if !versionSeen {
			t.Fatalf("app build did not drive planned version: %+v", destination)
		}
	}
	opts.Method = "apply"
	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	derived := readEnrichedPkginfo(t, filepath.Join(root, "derived"))
	if derived["version"] != "100" || derived["installer_item_hash"] != wantDigest {
		t.Fatalf("wrong derived pkginfo: %#v", derived)
	}
	installs, ok := derived["installs"].([]any)
	if !ok || len(installs) != 1 {
		t.Fatalf("missing derived application detection: %#v", derived)
	}
	app := installs[0].(map[string]any)
	if app["CFBundleIdentifier"] != "org.example.app" || app["CFBundleShortVersionString"] != "1.2" || app["CFBundleVersion"] != "100" || app["path"] != "/Applications/Example.app" {
		t.Fatalf("derived detection lost observed versions: %#v", app)
	}
	receipts := derived["receipts"].([]any)
	if len(receipts) != 1 || receipts[0].(map[string]any)["version"] != "9" {
		t.Fatalf("derived receipt version changed: %#v", receipts)
	}
	manifest = strings.Replace(manifest, "unattended_install: true", "unattended_install: false", 1)
	manifest = strings.Replace(manifest, "description: previous", "description: null", 1)
	if err := testproject.Write(configPath, []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	cleared, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Software[0].Steps[0].Cached || !reflect.DeepEqual(cleared.Software[0].Steps[0].Artifacts["artifact"].Facts, observed.Facts) {
		t.Fatal("metadata edit changed or recomputed observed facts")
	}
	authored := readEnrichedPkginfo(t, filepath.Join(root, "authored"))
	if authored["unattended_install"] != false {
		t.Fatalf("explicit false was lost: %#v", authored)
	}
	if _, exists := authored["description"]; exists {
		t.Fatalf("explicit null did not clear existing description: %#v", authored)
	}
	wantReceipts := []any{map[string]any{"packageid": "org.example.authored", "version": "6"}}
	wantInstalls := []any{map[string]any{"type": "file", "path": "/Library/Example/installed"}}
	if !reflect.DeepEqual(authored["receipts"], wantReceipts) || !reflect.DeepEqual(authored["installs"], wantInstalls) {
		t.Fatalf("derived metadata augmented explicit lists: %#v", authored)
	}
	for _, name := range []string{"derived", "authored"} {
		info := readEnrichedPkginfo(t, filepath.Join(root, name))
		published, err := os.ReadFile(filepath.Join(root, name, "pkgs", info["installer_item_location"].(string)))
		if err != nil || !bytes.Equal(published, installer) {
			t.Fatalf("%s did not retain vendor package: %v", name, err)
		}
	}
}

func TestInvalidSubjectBlocksOnlyItsDestination(t *testing.T) {
	for _, test := range []struct {
		name, match string
	}{
		{"missing", "matched 0"},
		{"ambiguous", "matched 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeEnrichmentApp(t, filepath.Join(root, "inputs/Payload"))
			if test.name == "ambiguous" {
				for _, name := range []string{"First.app", "Second.app"} {
					writeEnrichmentApp(t, filepath.Join(root, "inputs/apps", name))
				}
			}
			manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: readiness
spec:
  destinations:
    first: {operation: munki, config: {path: first}}
    second: {operation: munki, config: {path: second}}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: example
spec:
  source: {type: file, path: inputs}
  subjects:
    main: {bundle_id: org.example.app}
  artifacts:
    package:
      type: pkg
      identifier: org.example.receipt
      version: "9"
      payload: Payload
      install_location: /Applications/Example.app
  destinations:
    first: {installer: artifacts/package, pkginfo: {version: "9"}}
    second: {pkginfo: {version: {$fact: main.app.build}}}
`
			configPath := filepath.Join(root, "stemma.yaml")
			if err := testproject.Write(configPath, []byte(manifest)); err != nil {
				t.Fatal(err)
			}
			report, err := Run(t.Context(), Options{ConfigPath: configPath, CacheDir: t.TempDir(), Method: "apply"})
			if err == nil || !strings.Contains(err.Error(), test.match) || len(report.Software) != 1 || report.Software[0].Error == "" {
				t.Fatalf("subject failure was not content-dependent: %+v, %v", report, err)
			}
			if strings.Contains(err.Error(), "destination first") {
				t.Fatalf("first destination was not independently ready: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "second")); !os.IsNotExist(err) {
				t.Fatalf("invalid subject allowed publication: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "first")); err != nil {
				t.Fatalf("unrelated destination failed to publish: %v", err)
			}
		})
	}
}

func buildEnrichmentPackage(t *testing.T, project string) {
	t.Helper()
	root := t.TempDir()
	writeEnrichmentApp(t, filepath.Join(root, "Payload"))
	if err := pkgbuild.Build(t.Context(), root, filepath.Join(project, "vendor.pkg"), pkgbuild.Options{Identifier: "org.example.receipt", Version: "9", Payload: "Payload", InstallLocation: "/Applications/Example.app", Timestamp: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
}

func writeEnrichmentApp(t *testing.T, root string) {
	t.Helper()
	data, err := plist.Marshal(map[string]any{"CFBundleIdentifier": "org.example.app", "CFBundleName": "Example", "CFBundleShortVersionString": "1.2", "CFBundleVersion": "100", "CFBundleExecutable": "Example", "LSMinimumSystemVersion": "13.0"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	writeEnrichmentFile(t, filepath.Join(root, "Contents/Info.plist"), data)
	executable, err := os.ReadFile("../apple/testdata/Fixture.app/Contents/MacOS/fixture")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "Contents/MacOS/Example")
	writeEnrichmentFile(t, name, executable)
	if err := os.Chmod(name, 0755); err != nil {
		t.Fatal(err)
	}
}

func writeEnrichmentFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func readEnrichedPkginfo(t *testing.T, root string) map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "pkgsinfo/stemma/*/*.plist"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("expected one published pkginfo: %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if _, err := plist.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	return info
}
