package engine

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/testproject"
	"go.yaml.in/yaml/v4"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
)

func TestExecutableRegistersPreparationAndReconciliation(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "echo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write([]byte("fixture content"))
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
    remote: {operation: echo.reconcile, config: {}}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: fixture
spec:
  source: {type: http, url: %s/payload.bin}
  steps:
    - name: first
      operation: echo.inspect
      inputs: {payload: source}
    - name: inspected
      operation: inspect
      inputs: {input: first/payload}
    - name: second
      operation: echo.inspect
      inputs: {finished: inspected/artifact}
  destinations:
    remote: {artifact: second/finished, displayName: original}
`, server.URL)
	path := filepath.Join(root, "stemma.yaml")
	write := func(data string) {
		t.Helper()
		if err := testproject.Write(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	write(manifest)
	project, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	installFixturePlugin(t, store, root, binary, "original resource")
	if _, err := lockfile.Prepare(t.Context(), project, source.New(store, root, false), lockfile.Options{PluginsOnly: true}); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: path, CacheDir: store.Dir, Method: "plan"}
	if _, err := ValidateProject(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	catalog, err := Catalog(t.Context(), opts)
	if err != nil || len(catalog.Operations) != 8 || downloads.Load() != 0 {
		t.Fatalf("catalog acquired software or lost operations: %+v %v downloads=%d", catalog, err, downloads.Load())
	}
	first, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Software) != 1 || len(first.Software[0].Steps) != 3 || first.Software[0].Steps[2].Artifacts["finished"].Payload != first.Software[0].Prepared.Source.Artifact {
		t.Fatalf("named step outputs were not preserved: %+v", first)
	}
	write(strings.Replace(manifest, "displayName: original", "displayName: edited", 1))
	second, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range second.Software[0].Steps {
		if !step.Cached {
			t.Fatalf("metadata edit invalidated step %s", step.Name)
		}
	}
	if downloads.Load() != 1 {
		t.Fatal("warm operation run redownloaded source")
	}
	if err := os.RemoveAll(store.Dir); err != nil {
		t.Fatal(err)
	}
	store, err = cas.Open(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	installFixturePlugin(t, store, root, binary, "original resource")
	third, err := Run(t.Context(), opts)
	if err != nil || third.Software[0].Steps[2].Artifacts["finished"].Payload != first.Software[0].Steps[2].Artifacts["finished"].Payload {
		t.Fatalf("cold operation output changed: %+v %v", third, err)
	}
	installFixturePlugin(t, store, root, binary, "changed resource")
	changed, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Software[0].Steps[0].Cached {
		t.Fatal("resource-only bundle change reused plugin step cache")
	}
	// A second provider advertising the same names cannot shadow the first.
	write(strings.Replace(manifest, "  plugins:\n", "  plugins:\n    other:\n      trusted: true\n      image: registry.example/plugins/echo:v1\n", 1))
	project, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	installFixturePlugin(t, store, root, binary, "changed resource")
	if _, err := lockfile.Prepare(t.Context(), project, source.New(store, root, false), lockfile.Options{PluginsOnly: true}); err != nil {
		t.Fatal(err)
	}
	count := downloads.Load()
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("provider collision: %v", err)
	}
	if downloads.Load() != count {
		t.Fatal("collision discovered after software acquisition")
	}
}

func installFixturePlugin(t *testing.T, store *cas.Store, root, binary, resource string) {
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
	index := put(ocispec.MediaTypeImageIndex, data)
	entry := plugins.Entry{Image: "registry.example/plugins/echo:v1", Digest: index.Digest.String(), Size: index.Size}
	locked, err := lockfile.Load(filepath.Join(root, "stemma.lock.yaml"))
	if err != nil {
		locked = lockfile.File{Version: 1, Software: map[string]source.Entry{}}
	}
	locked.Plugins = map[string]plugins.Entry{"provider": entry, "other": entry}
	data, err = yaml.Marshal(locked)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stemma.lock.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
