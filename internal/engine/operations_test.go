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
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
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

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
)

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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { downloads.Add(1); _, _ = w.Write(served.Load().([]byte)) }))
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
			if transport == "path" {
				manifest = strings.Replace(manifest, "image: registry.example/plugins/echo:v1", "path: local-plugin", 1)
			}
			write(manifest)
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			install := func() {
				if transport == "image" {
					installFixturePlugin(t, store, root, binary, "original resource")
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
			opts := Options{ConfigPath: filename, CacheDir: store.Dir, Method: "prepare"}
			if _, err := ValidateProject(t.Context(), opts); err != nil {
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
			if _, err := Run(t.Context(), opts); err != nil {
				t.Fatal(err)
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
		locked = lockfile.File{Version: 2, Inputs: map[string]map[string]source.Entry{}}
	}
	locked.Plugins = map[string]plugins.Entry{"provider": entry}
	data, err = yaml.Marshal(locked)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stemma.lock.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
