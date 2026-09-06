package plugins

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woodleighschool/stemma/internal/cas"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestRegistryCredentialsPinnedRecoveryAndIntegrity(t *testing.T) {
	target := memory.New()
	selected, layer := fixtureManifest(t, target, "linux", "amd64", fixtureArchive(t, "plugin", "original", false))
	other, otherLayer := fixtureManifest(t, target, "darwin", "arm64", fixtureArchive(t, "plugin", "other", false))
	index := fixtureIndex(t, target, selected, other)
	replacement := fixtureIndex(t, target, other)
	descriptors := map[string]ocispec.Descriptor{}
	var visit func(ocispec.Descriptor)
	visit = func(desc ocispec.Descriptor) {
		descriptors[desc.Digest.String()] = desc
		children, err := content.Successors(t.Context(), target, desc)
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range children {
			visit(child)
		}
	}
	visit(index)
	visit(replacement)
	if err := target.Tag(t.Context(), index, "v1"); err != nil {
		t.Fatal(err)
	}
	var corrupt atomic.Bool
	var challenges atomic.Int64
	var mu sync.Mutex
	requests := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		if user != "fixture" || password != "synthetic-password" {
			challenges.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		requests[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		desc, exists := descriptors[ref]
		var err error
		if !exists {
			desc, err = target.Resolve(r.Context(), ref)
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		data, err := content.FetchAll(r.Context(), target, desc)
		if err != nil {
			http.Error(w, "fixture unavailable", 500)
			return
		}
		if corrupt.Load() && desc.Digest == layer.Digest {
			data[0] ^= 0xff
		}
		w.Header().Set("Content-Type", desc.MediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Docker-Content-Digest", desc.Digest.String())
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "https://")
	dockerConfig := t.TempDir()
	credential, err := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("fixture:synthetic-password"))}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfig)
	cache, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cache, false)
	s.platform = *selected.Platform
	s.registry = func(image string) (oras.ReadOnlyTarget, error) {
		target, err := repository(image)
		if err != nil {
			return nil, err
		}
		repo := target.(*remote.Repository)
		repo.Client.(*auth.Client).Client = server.Client()
		return repo, nil
	}
	image := host + "/plugin:v1"
	entry, err := s.Resolve(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := s.Acquire(t.Context(), image, entry)
	if err != nil {
		t.Fatal(err)
	}
	if challenges.Load() == 0 {
		t.Fatal("registry credential challenge was not exercised")
	}
	if err := target.Tag(t.Context(), replacement, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := cache.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	corrupt.Store(true)
	if _, err := s.Acquire(t.Context(), image, entry); err == nil {
		t.Fatal("corrupt registry response was accepted")
	}
	corrupt.Store(false)
	if recovered, err := s.Acquire(t.Context(), image, entry); err != nil || recovered != bundle {
		t.Fatalf("pinned recovery=%+v error=%v", recovered, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["HEAD /v2/plugin/manifests/v1"] != 1 || requests["GET /v2/plugin/manifests/"+other.Digest.String()] != 0 || requests["GET /v2/plugin/blobs/"+otherLayer.Digest.String()] != 0 {
		t.Fatalf("unexpected registry requests: %v", requests)
	}
}

func TestPinnedPlatformBundleSurvivesTagMovementAndCacheLoss(t *testing.T) {
	ctx := t.Context()
	remote := &recordedTarget{ReadOnlyTarget: memory.New(), fetches: map[digest.Digest]int{}}
	target := remote.ReadOnlyTarget.(*memory.Store)
	linux, linuxBlob := fixtureManifest(t, target, "linux", "amd64", fixtureArchive(t, "plugin", "original", false))
	darwin, darwinBlob := fixtureManifest(t, target, "darwin", "arm64", fixtureArchive(t, "plugin", "darwin", false))
	index := fixtureIndex(t, target, linux, darwin)
	const image = "registry.example/plugin:v1"
	if err := target.Tag(ctx, index, image); err != nil {
		t.Fatal(err)
	}
	cache, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cache, false)
	s.platform = ocispec.Platform{OS: "linux", Architecture: "amd64"}
	s.registry = func(string) (oras.ReadOnlyTarget, error) { return remote, nil }
	entry, err := s.Resolve(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := s.Acquire(ctx, image, entry)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Manifest != linux.Digest.String() || remote.fetches[darwin.Digest] != 0 || remote.fetches[darwinBlob.Digest] != 0 {
		t.Fatalf("downloaded the wrong platform: %+v fetches=%v", bundle, remote.fetches)
	}
	executable, err := s.Materialize(ctx, bundle, filepath.Join(t.TempDir(), "installed"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil || string(data) != "original" {
		t.Fatalf("entrypoint=%q error=%v", data, err)
	}
	resource, err := os.ReadFile(filepath.Join(filepath.Dir(executable), "resources", "message.txt"))
	if err != nil || string(resource) != "original resource" {
		t.Fatalf("resource=%q error=%v", resource, err)
	}
	changed, _ := fixtureManifest(t, target, "linux", "amd64", fixtureArchive(t, "plugin", "changed", false))
	newIndex := fixtureIndex(t, target, changed, darwin)
	if err := target.Tag(ctx, newIndex, image); err != nil {
		t.Fatal(err)
	}
	s.offline = true
	if warm, err := s.Acquire(ctx, image, entry); err != nil || warm != bundle {
		t.Fatalf("offline bundle=%+v error=%v", warm, err)
	}
	if remote.resolves != 1 || remote.fetches[linuxBlob.Digest] != 1 {
		t.Fatal("warm run contacted registry")
	}
	path, err := cache.Path(bundle.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(bundle.Artifact.Size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, image, entry); err == nil {
		t.Fatal("offline cache corruption went unnoticed")
	}
	s.offline = false
	if err := cache.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if cold, err := s.Acquire(ctx, image, entry); err != nil || cold != bundle {
		t.Fatalf("cold run followed moved tag: %+v %v", cold, err)
	}
	if remote.resolves != 1 {
		t.Fatal("cold recovery resolved a mutable tag")
	}
	updated, err := s.Resolve(ctx, image)
	if err != nil || updated.Digest == entry.Digest {
		t.Fatalf("explicit update=%+v error=%v", updated, err)
	}
	s.platform.OS, s.platform.Architecture = "windows", "arm64"
	if _, err := s.Acquire(ctx, image, entry); err == nil || !strings.Contains(err.Error(), "no bundle") {
		t.Fatalf("unsupported platform: %v", err)
	}
	if _, err := s.Acquire(ctx, image+"-changed", entry); err == nil {
		t.Fatal("changed image reused stale lock")
	}
}

func TestBundleEntrypointAndExtractionContract(t *testing.T) {
	for _, test := range []struct {
		name, entrypoint string
		link, valid      bool
	}{
		{"windows", "plugin.exe", false, true},
		{"missing", "different.exe", false, false},
		{"symlink", "plugin.exe", true, false},
		{"traversal", "../plugin.exe", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ref, err := cache.Import(t.Context(), bytes.NewReader(fixtureArchive(t, test.entrypoint, "payload", test.link)), "")
			if err != nil {
				t.Fatal(err)
			}
			s := New(cache, true)
			s.platform.OS = "windows"
			destination := filepath.Join(t.TempDir(), "bundle")
			_, err = s.Materialize(t.Context(), Bundle{Artifact: ref}, destination)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
			if !test.valid {
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatalf("failed extraction left a partial bundle: %v", err)
				}
			}
		})
	}
}

func TestRejectAmbiguousIndexAndInvalidManifest(t *testing.T) {
	for _, test := range []string{"duplicate-platform", "wrong-artifact", "extra-layer", "wrong-layer", "bad-digest", "oversized-index"} {
		t.Run(test, func(t *testing.T) {
			target := memory.New()
			manifest, _ := fixtureManifest(t, target, "linux", "amd64", fixtureArchive(t, "plugin", "payload", false))
			data, err := content.FetchAll(t.Context(), target, manifest)
			if err != nil {
				t.Fatal(err)
			}
			var value ocispec.Manifest
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			switch test {
			case "wrong-artifact":
				value.ArtifactType = "application/other"
			case "extra-layer":
				value.Layers = append(value.Layers, value.Layers[0])
			case "wrong-layer":
				value.Layers[0].MediaType = "application/zip"
			case "bad-digest":
				value.Layers[0].Digest = digest.Digest("sha256:" + strings.Repeat("0", 64))
			}
			platform := manifest.Platform
			manifest = putJSON(t, target, ocispec.MediaTypeImageManifest, value)
			manifest.Platform = platform
			manifests := []ocispec.Descriptor{manifest}
			if test == "duplicate-platform" {
				manifests = append(manifests, manifest)
			}
			index := fixtureIndex(t, target, manifests...)
			cache, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := New(cache, false)
			s.platform = *platform
			s.registry = func(string) (oras.ReadOnlyTarget, error) { return target, nil }
			entry := Entry{Image: "registry.example/plugin:v1", Digest: index.Digest.String(), Size: index.Size}
			if test == "oversized-index" {
				entry.Size = maxMetadataSize + 1
			}
			if _, err := s.Acquire(t.Context(), entry.Image, entry); err == nil {
				t.Fatal("invalid package accepted")
			}
		})
	}
}

type recordedTarget struct {
	oras.ReadOnlyTarget
	fetches  map[digest.Digest]int
	resolves int
}

func (r *recordedTarget) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	r.fetches[desc.Digest]++
	return r.ReadOnlyTarget.Fetch(ctx, desc)
}

func (r *recordedTarget) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	r.resolves++
	return r.ReadOnlyTarget.Resolve(ctx, ref)
}

func fixtureManifest(t *testing.T, target *memory.Store, goos, goarch string, bundle []byte) (ocispec.Descriptor, ocispec.Descriptor) {
	t.Helper()
	layer := content.NewDescriptorFromBytes(BundleType, bundle)
	if err := target.Push(t.Context(), layer, bytes.NewReader(bundle)); err != nil {
		t.Fatal(err)
	}
	manifest, err := oras.PackManifest(t.Context(), target, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{Layers: []ocispec.Descriptor{layer}, ManifestAnnotations: map[string]string{ocispec.AnnotationCreated: "1970-01-01T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest.Platform = &ocispec.Platform{OS: goos, Architecture: goarch}
	return manifest, layer
}

func fixtureIndex(t *testing.T, target *memory.Store, manifests ...ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	return putJSON(t, target, ocispec.MediaTypeImageIndex, ocispec.Index{SchemaVersion: 2, MediaType: ocispec.MediaTypeImageIndex, Manifests: manifests})
}

func putJSON(t *testing.T, target *memory.Store, media string, value any) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	desc := content.NewDescriptorFromBytes(media, data)
	if err := target.Push(t.Context(), desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatal(err)
	}
	return desc
}

func fixtureArchive(t *testing.T, name, payload string, link bool) []byte {
	t.Helper()
	var compressed bytes.Buffer
	z, err := zstd.NewWriter(&compressed, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(z)
	header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(payload))}
	if link {
		header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, "resources/message.txt", 0
	}
	if err := w.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if !link {
		if _, err := io.WriteString(w, payload); err != nil {
			t.Fatal(err)
		}
	}
	resource := payload + " resource"
	if err := w.WriteHeader(&tar.Header{Name: "resources/message.txt", Mode: 0o644, Size: int64(len(resource))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, resource); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
