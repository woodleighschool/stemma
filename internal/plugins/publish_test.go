package plugins

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woodleighschool/stemma/internal/cas"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestPublishedGoReleaserReleaseRunsOnEachPlatform(t *testing.T) {
	platforms := []ocispec.Platform{{OS: "darwin", Architecture: "arm64"}, {OS: "linux", Architecture: "amd64"}, {OS: "windows", Architecture: "arm64"}}
	var artifacts []goreleaserArtifact
	files := map[string][]byte{}
	for _, p := range platforms {
		name := fmt.Sprintf("tools_1.0.0_%s_%s.tar.zst", p.OS, p.Architecture)
		artifacts = append(artifacts, archiveArtifact(name, p.OS, p.Architecture, "tar.zst"))
		files[name] = fixtureArchive(t, entrypoint(p.OS), "tools for "+p.OS, false)
	}
	bundles, err := GoReleaserBundles(writeDist(t, artifacts, files))
	if err != nil {
		t.Fatal(err)
	}
	registry := &testRegistry{}
	server := httptest.NewTLSServer(registry)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "https://")
	dockerLogin(t, host)
	connect := func(image string) (*remote.Repository, error) {
		repo, err := repository(image)
		if err != nil {
			return nil, err
		}
		repo.Client.(*auth.Client).Client = server.Client()
		return repo, nil
	}
	image := host + "/plugin:1.0.0"
	repo, err := connect(image)
	if err != nil {
		t.Fatal(err)
	}
	annotations := map[string]string{ocispec.AnnotationSource: "https://example.org/tools"}
	published, err := publish(t.Context(), repo, "1.0.0", bundles, annotations)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range platforms {
		t.Run(p.OS, func(t *testing.T) {
			cache, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := New(cache, false)
			s.platform = p
			s.registry = func(image string) (oras.ReadOnlyTarget, error) {
				repo, err := connect(image)
				if err != nil {
					return nil, err
				}
				return repo, nil
			}
			entry, err := s.Resolve(t.Context(), image)
			if err != nil {
				t.Fatal(err)
			}
			if entry.Digest != published.Digest.String() {
				t.Fatalf("installed %s, published %s", entry.Digest, published.Digest)
			}
			bundle, err := s.Acquire(t.Context(), image, entry)
			if err != nil {
				t.Fatal(err)
			}
			executable, err := s.Materialize(t.Context(), bundle, filepath.Join(t.TempDir(), "bundle"))
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(executable); err != nil || string(data) != "tools for "+p.OS {
				t.Fatalf("entrypoint = %q, %v", data, err)
			}
		})
	}

	var index ocispec.Index
	if err := json.Unmarshal(registry.get(published.Digest), &index); err != nil {
		t.Fatal(err)
	}
	if index.Annotations[ocispec.AnnotationSource] != "https://example.org/tools" {
		t.Fatalf("index annotations = %v", index.Annotations)
	}
	// GoReleaser lists targets in the order its builds finish.
	slices.Reverse(bundles)
	uploads := registry.uploadCount()
	again, err := publish(t.Context(), repo, "1.0.0", bundles, annotations)
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest != published.Digest || registry.uploadCount() != uploads {
		t.Fatalf("republishing changed the release: %s -> %s, %d new uploads", published.Digest, again.Digest, registry.uploadCount()-uploads)
	}
}

func TestPublishRejectsBundlesTheLoaderCannotRun(t *testing.T) {
	linux := linuxBundle(t, fixtureArchive(t, "plugin", "tools", false))
	for _, test := range []struct {
		name    string
		bundles []PlatformBundle
	}{
		{"no bundles", nil},
		{"duplicate platform", []PlatformBundle{linux, linuxBundle(t, fixtureArchive(t, "plugin", "other", false))}},
		{"platform variant", []PlatformBundle{{Platform: ocispec.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}, Path: linux.Path}}},
		{"windows without plugin.exe", []PlatformBundle{{Platform: ocispec.Platform{OS: "windows", Architecture: "amd64"}, Path: linux.Path}}},
		{"linked entrypoint", []PlatformBundle{linuxBundle(t, fixtureArchive(t, "plugin", "", true))}},
		{"not Zstandard", []PlatformBundle{linuxBundle(t, []byte("PK\x03\x04 zip bytes"))}},
		{"missing file", []PlatformBundle{{Platform: linux.Platform, Path: filepath.Join(t.TempDir(), "tools.tar.zst")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := memory.New()
			if _, err := publish(t.Context(), target, "1.0.0", test.bundles, nil); err == nil {
				t.Fatal("unusable release published")
			}
			if exists, err := target.Exists(t.Context(), ocispec.DescriptorEmptyJSON); err != nil || exists {
				t.Fatalf("rejected release pushed content: %v", err)
			}
		})
	}
}

func TestFailedPublicationKeepsTheReleaseTag(t *testing.T) {
	target := memory.New()
	released, err := publish(t.Context(), target, "1.0.0", []PlatformBundle{linuxBundle(t, fixtureArchive(t, "plugin", "released", false))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejecting := rejectingTarget{Target: target, mediaType: ocispec.MediaTypeImageManifest}
	_, err = publish(t.Context(), rejecting, "1.0.0", []PlatformBundle{linuxBundle(t, fixtureArchive(t, "plugin", "rebuilt", false))}, nil)
	if !errors.Is(err, errRejected) {
		t.Fatalf("error = %v, want the registry's rejection", err)
	}
	tagged, err := target.Resolve(t.Context(), "1.0.0")
	if err != nil || tagged.Digest != released.Digest {
		t.Fatalf("tag = %s, %v; want %s", tagged.Digest, err, released.Digest)
	}
}

func TestPublishRequiresATaggedImage(t *testing.T) {
	for _, image := range []string{"registry.example/plugin", "registry.example/plugin@sha256:" + strings.Repeat("0", 64)} {
		if _, err := Publish(t.Context(), image, nil, nil); err == nil || !strings.Contains(err.Error(), "tag") {
			t.Fatalf("%s: error = %v", image, err)
		}
	}
}

func linuxBundle(t *testing.T, data []byte) PlatformBundle {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools_linux_amd64.tar.zst")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return PlatformBundle{Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"}, Path: path}
}

var errRejected = errors.New("registry rejected the push")

type rejectingTarget struct {
	oras.Target

	mediaType string
}

func (r rejectingTarget) Push(ctx context.Context, desc ocispec.Descriptor, content io.Reader) error {
	if desc.MediaType == r.mediaType {
		return errRejected
	}
	return r.Target.Push(ctx, desc, content)
}

// testRegistry serves the distribution API calls ORAS makes to push and pull
// the repository "plugin", challenging requests without the fixture
// credentials.
type testRegistry struct {
	mu        sync.Mutex
	content   map[digest.Digest][]byte
	mediaType map[digest.Digest]string
	tags      map[string]digest.Digest
	uploads   int
}

func (r *testRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if user, password, _ := req.BasicAuth(); user != "fixture" || password != "synthetic-password" {
		w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.content == nil {
		r.content, r.mediaType, r.tags = map[digest.Digest][]byte{}, map[digest.Digest]string{}, map[string]digest.Digest{}
	}
	kind, ref, _ := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v2/plugin/"), "/")
	switch {
	case req.Method == http.MethodPost && kind == "blobs" && ref == "uploads/":
		r.uploads++
		w.Header().Set("Location", "/v2/plugin/blobs/uploads/session")
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPut:
		data, err := io.ReadAll(req.Body)
		want := digest.Digest(req.URL.Query().Get("digest"))
		if kind == "manifests" {
			want = digest.FromBytes(data)
		}
		if err != nil || digest.FromBytes(data) != want {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		r.content[want] = data
		if kind == "manifests" {
			r.mediaType[want] = req.Header.Get("Content-Type")
			if digest.Digest(ref).Validate() != nil {
				r.tags[ref] = want
			}
		}
		w.Header().Set("Docker-Content-Digest", want.String())
		w.WriteHeader(http.StatusCreated)
	case req.Method == http.MethodGet || req.Method == http.MethodHead:
		d, tagged := r.tags[ref]
		if !tagged || kind != "manifests" {
			d = digest.Digest(ref)
		}
		data, ok := r.content[d]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", cmp.Or(r.mediaType[d], "application/octet-stream"))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Docker-Content-Digest", d.String())
		if req.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	default:
		http.NotFound(w, req)
	}
}

func (r *testRegistry) get(d digest.Digest) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.content[d]
}

func (r *testRegistry) uploadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uploads
}
