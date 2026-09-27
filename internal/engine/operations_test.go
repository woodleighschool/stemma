package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"

	"github.com/woodleighschool/stemma/internal/cas"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/plugin"
)

func TestExternalResolverEvidenceFeedsNativeMetadata(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "local-plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(pluginDir, "plugin")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	payload, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	var version, revision atomic.Value
	version.Store("1.2")
	revision.Store("first")
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			downloads.Add(1)
		}
		w.Header().Set("X-Fixture-Version", version.Load().(string))
		w.Header().Set("X-Fixture-Revision", revision.Load().(string))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: evidence}
spec:
  imports: ['*.software.yaml']
  plugins:
    provider: {trusted: true, path: local-plugin}
  destinations:
    repo: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: fixture}
spec:
  source: {resolver: echo.download, url: %s/vendor.pkg}
  destinations:
    repo:
      pkginfo:
        version: "{{ evidence['vendor.release'].version }}"
`, server.URL))
	wantVersion := "1.2"
	validated := false
	opts := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "prepare", Handlers: map[string]reconcileHandler{
		"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
			if request.Prepared {
				var metadata struct {
					Pkginfo struct {
						Version string `json:"version"`
					} `json:"pkginfo"`
				}
				if err := json.Unmarshal(request.Metadata, &metadata); err != nil {
					t.Fatal(err)
				}
				if metadata.Pkginfo.Version != wantVersion {
					t.Fatalf("destination version %q, want reviewed evidence %q", metadata.Pkginfo.Version, wantVersion)
				}
				validated = true
			}
			return plugin.ReconcileResponse{}, nil
		},
	}}
	run := func() ResourceReport {
		t.Helper()
		validated = false
		report, err := Run(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Resources) != 1 || !validated {
			t.Fatalf("resolver evidence did not reach destination validation: %+v", report)
		}
		return report.Resources[0]
	}
	update := func() {
		t.Helper()
		if _, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: opts.CacheDir, Method: "update"}); err != nil {
			t.Fatal(err)
		}
	}
	update()
	first := run()
	installer := first.Artifacts["installer"]
	if got := string(installer.Evidence["vendor.release"]); got != `{"version":"1.2"}` {
		t.Fatalf("MacSoftware dropped resolver evidence: %s", got)
	}
	version.Store("2.0")
	opts.Lock = lockfile.Options{Offline: true}
	if warm := run(); !warm.Cached || downloads.Load() != 1 {
		t.Fatal("warm offline preparation did not reuse reviewed evidence")
	}
	if err := os.RemoveAll(opts.CacheDir); err != nil {
		t.Fatal(err)
	}
	opts.Lock.Offline = false
	if cold := run(); cold.Cached || downloads.Load() != 2 || cold.Artifacts["installer"].InputsHash != installer.InputsHash {
		t.Fatal("cold fetch changed reviewed preparation inputs")
	}
	wantVersion = "2.0"
	update()
	refreshed := run()
	updated := refreshed.Artifacts["installer"]
	if refreshed.Cached || updated.InputsHash == installer.InputsHash || updated.Payload != installer.Payload {
		t.Fatal("evidence refresh did not invalidate preparation while retaining byte identity")
	}
	revision.Store("second")
	update()
	if observed := run(); !observed.Cached || observed.Artifacts["installer"].InputsHash != updated.InputsHash {
		t.Fatal("private observation change invalidated preparation")
	}
}

func TestExternalResolverBuilderAndNativeDestination(t *testing.T) {
	for _, transport := range []string{"image", "path"} {
		t.Run(transport, func(t *testing.T) {
			root := t.TempDir()
			binary := filepath.Join(root, "echo")
			build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build plugin: %v\n%s", err, output)
			}
			payload, err := os.ReadFile("../apple/testdata/fixture.pkg")
			if err != nil {
				t.Fatal(err)
			}
			var served atomic.Value
			served.Store(payload)
			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					downloads.Add(1)
				}
				_, _ = w.Write(served.Load().([]byte))
			}))
			defer server.Close()
			manifest := fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: operations
spec:
  plugins:
    provider:
      trusted: true
      image: registry.example/plugins/echo:v1
  destinations:
    local:
      operation: munki
      config:
        path: repo
  imports:
    - '*.software.yaml'
---
apiVersion: example.test/v1
kind: ExternalInstaller
metadata:
  name: fixture-builder
spec:
  source:
    resolver: echo.download
    url: %s/vendor.pkg
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: fixture
spec:
  source:
    resource:
      apiVersion: example.test/v1
      kind: ExternalInstaller
      name: fixture-builder
      output: installer
  destinations:
    local:
      pkginfo:
        description: original
`, server.URL)
			filename := filepath.Join(root, "stemma.yaml")
			write := func(text string) {
				t.Helper()
				testproject.Write(t, filename, text)
			}
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var index string
			install := func() {
				if transport == "image" {
					index = installFixturePlugin(t, store, binary, "original resource")
					return
				}
				directory := filepath.Join(root, "local-plugin")
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(binary)
				if err != nil {
					t.Fatal(err)
				}
				name := "plugin"
				if runtime.GOOS == "windows" {
					name += ".exe"
				}
				if err := os.WriteFile(filepath.Join(directory, name), data, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "resource.txt"), []byte("original resource"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			install()
			// An image names its digest; a local path is locked by updating plugins.
			manifest = strings.Replace(manifest, "image: registry.example/plugins/echo:v1", "image: registry.example/plugins/echo:v1@"+index, 1)
			if transport == "path" {
				manifest = strings.Replace(manifest, "image: registry.example/plugins/echo:v1@", "path: local-plugin", 1)
			}
			write(manifest)
			opts := Options{ConfigPath: filename, CacheDir: store.Dir, Method: "update"}
			if transport == "path" {
				if _, err := UpdatePlugins(t.Context(), opts, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ValidateProject(t.Context(), opts, false); err != nil {
				t.Fatal(err)
			}
			descriptor, err := Catalog(t.Context(), opts)
			if err != nil || downloads.Load() != 0 {
				t.Fatalf("catalog acquired inputs: %v", err)
			}
			found := false
			for _, operation := range descriptor.Operations {
				if operation.Resource != nil && operation.Resource.Kind == "ExternalInstaller" {
					found = true
				}
			}
			if !found {
				t.Fatal("external kind registration missing")
			}
			t.Setenv("STEMMA_ECHO_REQUIRE_TOOL", "stemma-fixture-missing-helper-8ab22")
			if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "install the fixture helper") {
				t.Fatalf("missing runner prerequisite: %v", err)
			}
			if downloads.Load() != 0 {
				t.Fatal("prerequisite failure acquired an input")
			}
			t.Setenv("STEMMA_ECHO_REQUIRE_TOOL", "")
			for _, method := range []string{"update", "prepare"} {
				opts.Method = method
				if _, err := Run(t.Context(), opts); err != nil {
					t.Fatal(err)
				}
			}
			opts.Method = "apply"
			first, err := Run(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Resources) != 2 || !first.Resources[1].Destinations[0].Applied {
				t.Fatalf("external artifact was not published by native consumer: %+v", first)
			}
			installer := first.Resources[0].Artifacts["installer"]
			if !bytes.Contains(installer.Evidence["vendor.probe"], []byte(`"revision":7`)) {
				t.Fatal("external evidence was dropped")
			}
			write(strings.Replace(manifest, "description: original", "description: edited", 1))
			second, err := Run(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range second.Resources {
				if !item.Cached {
					t.Fatalf("metadata invalidated %s", item.Kind)
				}
			}
			if downloads.Load() != 1 {
				t.Fatal("warm run reacquired locked input")
			}
			if err := os.RemoveAll(store.Dir); err != nil {
				t.Fatal(err)
			}
			store, err = cas.Open(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			install()
			third, err := Run(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if third.Resources[0].Artifacts["installer"].Payload != installer.Payload || len(third.Resources[1].Destinations[0].Changes) != 0 {
				t.Fatal("cache loss changed immutable output or replayed publication")
			}
			if downloads.Load() != 2 {
				t.Fatal("cold external resolver was not exercised")
			}
			// Locked consumption cannot accept changed bytes at the observation URL.
			served.Store([]byte("changed upstream content"))
			if err := os.RemoveAll(store.Dir); err != nil {
				t.Fatal(err)
			}
			store, err = cas.Open(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			install()
			if _, err := Run(t.Context(), opts); err == nil {
				t.Fatal("locked resolver silently substituted changed upstream bytes")
			}
		})
	}
}

// installFixturePlugin caches a platform index holding binary and returns its digest.
func installFixturePlugin(t *testing.T, store *cas.Store, binary, resource string) string {
	t.Helper()
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	name := "plugin"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var bundle bytes.Buffer
	compressed, err := zstd.NewWriter(&bundle, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(compressed)
	for _, file := range []struct {
		name string
		data []byte
	}{{name, data}, {"resource.txt", []byte(resource)}} {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: 0o755, Size: int64(len(file.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	put := func(media string, data []byte) ocispec.Descriptor {
		t.Helper()
		ref, err := store.Import(t.Context(), bytes.NewReader(data), "")
		if err != nil {
			t.Fatal(err)
		}
		return ocispec.Descriptor{MediaType: media, Digest: digest.Digest("sha256:" + ref.SHA256), Size: ref.Size}
	}
	config := put(ocispec.MediaTypeEmptyJSON, []byte("{}"))
	layer := put(plugins.BundleType, bundle.Bytes())
	data, err = json.Marshal(ocispec.Manifest{SchemaVersion: 2, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: plugins.ArtifactType, Config: config, Layers: []ocispec.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := put(ocispec.MediaTypeImageManifest, data)
	manifest.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	data, err = json.Marshal(ocispec.Index{SchemaVersion: 2, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{manifest}})
	if err != nil {
		t.Fatal(err)
	}
	return put(ocispec.MediaTypeImageIndex, data).Digest.String()
}

// TestStaleInterfaceFailsOnlyWhereUsed loads a plugin whose reconcile
// interface is another version: its resolver stays usable, so runs that never
// reach a destination succeed, while every command that checks or publishes to
// its destination fails with the reason.
func TestStaleInterfaceFailsOnlyWhereUsed(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "local-plugin", "plugin")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	payload, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: stale}
spec:
  imports: ['*.software.yaml']
  plugins:
    provider: {trusted: true, path: local-plugin}
  destinations:
    remote: {operation: echo.reconcile}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: fetched}
spec:
  source: {resolver: echo.download, url: %s/vendor.pkg}
  destinations:
    remote: {}
`, server.URL))
	t.Setenv("STEMMA_ECHO_STALE_KIND", "reconcile")
	const reason = "operation echo.reconcile is unavailable: plugin provider implements reconcile interface 2; this Stemma uses 1"
	opts := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	report, err := Run(t.Context(), opts)
	if err != nil || len(report.Resources) != 1 || len(report.Resources[0].Inputs) != 1 {
		t.Fatalf("update beside a stale destination: report=%+v err=%v", report, err)
	}
	opts.Method, opts.Resources = "artifact", []string{"MacSoftware/fetched"}
	if report, err := Run(t.Context(), opts); err != nil || report.Artifact == "" {
		t.Fatalf("artifact beside a stale destination: report=%+v err=%v", report, err)
	}
	opts.Method, opts.Resources = "prepare", nil
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("preparation checked against the stale destination: %v", err)
	}
	if _, err := ValidateProject(t.Context(), opts, false); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("validation of the stale destination: %v", err)
	}
	if _, err := ProjectSchema(t.Context(), opts); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("schema without the stale destination: %v", err)
	}
}
