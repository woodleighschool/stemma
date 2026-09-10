package engine

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testproject"
	"howett.net/plist"
)

func TestZIPAppPackageAndMunkiMetadataFollowLockedVendorVersion(t *testing.T) {
	var upstream atomic.Value
	firstZIP := zipApp(t, "1.2", "100")
	upstream.Store(firstZIP)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Example.zip" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(upstream.Load().([]byte))
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	configPath := filepath.Join(root, "stemma.yaml")
	manifest := fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: zip-package
spec:
  imports: ['*.software.yaml']
  destinations:
    local: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: example
spec:
  source: {type: http, url: %s/Example.zip}
  select: Example.app
  subjects:
    app: {kind: app, bundle_id: org.example.app}
  steps:
    - name: package
      operation: pkg
      inputs: {input: prepared}
      config:
        identifier: org.example.app
        version: {$fact: app.app.version}
        payload: .
        install_location: /Applications/Example.app
    - name: metadata
      operation: munki.pkginfo
      inputs: {input: package/artifact}
      config:
        name: Example
        unattended_install: true
        uninstallable: true
        uninstall_method: removepackages
  destinations:
    local:
      installer: metadata/artifact
      retention: {keep: 1}
      inputs: {installer: package/artifact}
      pkginfo: {catalogs: [testing]}
`, server.URL)
	if err := testproject.Write(configPath, []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: configPath, CacheDir: t.TempDir(), Method: "plan"}
	run := func() Report {
		t.Helper()
		report, err := Run(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	first := run()
	if _, err := os.Stat(filepath.Join(root, "repo")); !os.IsNotExist(err) {
		t.Fatalf("planning wrote the destination: %v", err)
	}
	locked, err := lockfile.Load(filepath.Join(root, "stemma.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	entry := locked.Software["example"]
	if entry.Artifact.SHA256 != fmt.Sprintf("%x", sha256.Sum256(firstZIP)) || entry.Filename != "Example.zip" || entry.ResolvedAt.IsZero() {
		t.Fatalf("source lock did not retain reviewed ZIP identity: %+v", entry)
	}
	opts.Method, opts.Lock = "apply", lockfile.Options{Frozen: true}
	applied := run()
	if applied.LockChanged || applied.Software[0].Steps[0].Artifacts["artifact"].Payload != first.Software[0].Steps[0].Artifacts["artifact"].Payload {
		t.Fatal("applying the reviewed plan changed package bytes or the source lock")
	}
	checkZIPPackage(t, root, "1.2", "100")
	upstream.Store(zipApp(t, "1.3", "101"))
	pinned := run()
	if pinned.LockChanged || len(pinned.Software[0].Destinations[0].Changes) != 0 {
		t.Fatal("frozen run adopted unreviewed upstream ZIP changes")
	}
	opts.Lock = lockfile.Options{Refresh: true}
	updated := run()
	if !updated.LockChanged || updated.Software[0].Steps[0].Artifacts["artifact"].Payload == first.Software[0].Steps[0].Artifacts["artifact"].Payload {
		t.Fatal("refreshed vendor version did not change the lock and package")
	}
	checkZIPPackage(t, root, "1.3", "101")
}

func checkZIPPackage(t *testing.T, root, version, build string) {
	t.Helper()
	info := readEnrichedPkginfo(t, filepath.Join(root, "repo"))
	if info["version"] != version || info["unattended_install"] != true || info["uninstallable"] != true || info["uninstall_method"] != "removepackages" {
		t.Fatalf("native pkginfo lost vendor version or installation policy: %#v", info)
	}
	installs, ok := info["installs"].([]any)
	if !ok || len(installs) != 1 {
		t.Fatalf("missing native application detection: %#v", info)
	}
	app := installs[0].(map[string]any)
	if app["type"] != "application" || app["path"] != "/Applications/Example.app" || app["CFBundleIdentifier"] != "org.example.app" || app["CFBundleShortVersionString"] != version || app["CFBundleVersion"] != build {
		t.Fatalf("incorrect native application detection: %#v", app)
	}
	receipts, ok := info["receipts"].([]any)
	if !ok || len(receipts) != 1 || receipts[0].(map[string]any)["packageid"] != "org.example.app" || receipts[0].(map[string]any)["version"] != version {
		t.Fatalf("receipt did not follow the vendor app version: %#v", info)
	}
	installerPath := filepath.Join(root, "repo/pkgs", info["installer_item_location"].(string))
	installer, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(installerPath) != ".pkg" || info["installer_item_hash"] != fmt.Sprintf("%x", sha256.Sum256(installer)) {
		t.Fatal("native metadata does not identify the published package bytes")
	}
	facts, err := apple.InspectPackageContents(t.Context(), installerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Packages) != 1 || facts.Packages[0].Version != version || facts.Packages[0].InstallLocation != "/Applications/Example.app" {
		t.Fatalf("package receipt has incorrect installation semantics: %+v", facts.Packages)
	}
	if len(facts.Applications) != 1 || facts.Applications[0].InstalledPath != "/Applications/Example.app" || facts.Applications[0].App.Version != version || facts.Applications[0].App.Build != build {
		t.Fatalf("package payload has incorrect app installation semantics: %+v", facts.Applications)
	}
}

func zipApp(t *testing.T, version, build string) []byte {
	t.Helper()
	root := t.TempDir()
	writeEnrichmentApp(t, filepath.Join(root, "Example.app"))
	infoPath := filepath.Join(root, "Example.app/Contents/Info.plist")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if _, err := plist.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	info["CFBundleShortVersionString"], info["CFBundleVersion"] = version, build
	data, err = plist.Marshal(info, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	writeEnrichmentFile(t, infoPath, data)
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	if err := w.AddFS(os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
