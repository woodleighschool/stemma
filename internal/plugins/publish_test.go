package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/oras-project/oras-go/v3"
	"github.com/oras-project/oras-go/v3/content"
	"github.com/oras-project/oras-go/v3/content/memory"
	"github.com/oras-project/oras-go/v3/errdef"
	"github.com/oras-project/oras-go/v3/registry/remote"
	"github.com/oras-project/oras-go/v3/registry/remote/auth"
	"github.com/woodleighschool/stemma/internal/cas"
)

func TestPublishedGoReleaserReleaseRunsOnEachPlatform(t *testing.T) {
	platforms := []ocispec.Platform{{OS: "darwin", Architecture: "arm64"}, {OS: "linux", Architecture: "amd64"}, {OS: "windows", Architecture: "arm64"}}
	var artifacts []artifactFixture
	files := map[string][]byte{}
	for _, p := range platforms {
		name := fmt.Sprintf("tools_1.0.0_%s_%s.tar.zst", p.OS, p.Architecture)
		artifacts = append(artifacts, archiveArtifact("plugin", name, p.OS, p.Architecture, "tar.zst"))
		files[name] = fixtureArchive(t, entrypoint(p.OS), "tools for "+p.OS, false)
	}
	goreleaserProject(t, artifacts, files)
	bundles, err := GoReleaserBundles("dist", "")
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	server := httptest.NewTLSServer(authenticated(&writes))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "https://")
	dockerLogin(t, host)
	connect := func(image string) (*remote.Repository, error) {
		repo, err := repository(image)
		if err != nil {
			return nil, err
		}
		repo.Registry.Client.(*auth.Client).Client = server.Client()
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

	data, err := content.FetchAll(t.Context(), repo, published)
	if err != nil {
		t.Fatal(err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if index.Annotations[ocispec.AnnotationSource] != "https://example.org/tools" {
		t.Fatalf("index annotations = %v", index.Annotations)
	}
	// GoReleaser lists targets in the order its builds finish.
	slices.Reverse(bundles)
	before := writes.Load()
	again, err := publish(t.Context(), repo, "1.0.0", bundles, annotations)
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest != published.Digest || writes.Load() != before {
		t.Fatalf("republishing changed the release: %s -> %s, %d new writes", published.Digest, again.Digest, writes.Load()-before)
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

func TestPublishedTagKeepsItsRelease(t *testing.T) {
	target := memory.New()
	released, err := publish(t.Context(), target, "1.0.0", []PlatformBundle{linuxBundle(t, fixtureArchive(t, "plugin", "released", false))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publish(t.Context(), target, "1.0.0", []PlatformBundle{linuxBundle(t, fixtureArchive(t, "plugin", "rebuilt", false))}, nil); err == nil {
		t.Fatal("rebuilt bundles replaced a published release")
	}
	tagged, err := target.Resolve(t.Context(), "1.0.0")
	if err != nil || tagged.Digest != released.Digest {
		t.Fatalf("tag = %s, %v; want %s", tagged.Digest, err, released.Digest)
	}
}

func TestInterruptedPublicationTagsNothingUntilRerun(t *testing.T) {
	target := memory.New()
	bundles := []PlatformBundle{linuxBundle(t, fixtureArchive(t, "plugin", "tools", false))}
	rejecting := rejectingTarget{Target: target, mediaType: ocispec.MediaTypeImageManifest}
	if _, err := publish(t.Context(), rejecting, "1.0.0", bundles, nil); !errors.Is(err, errRejected) {
		t.Fatalf("error = %v, want the registry's rejection", err)
	}
	if _, err := target.Resolve(t.Context(), "1.0.0"); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("interrupted publication tagged a release: %v", err)
	}
	published, err := publish(t.Context(), target, "1.0.0", bundles, nil)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := target.Resolve(t.Context(), "1.0.0")
	if err != nil || tagged.Digest != published.Digest {
		t.Fatalf("tag = %s, %v; want %s", tagged.Digest, err, published.Digest)
	}
}

func TestPublishRequiresATaggedImage(t *testing.T) {
	for _, image := range []string{"registry.example/plugin", "registry.example/plugin@sha256:" + strings.Repeat("0", 64), "oci://registry.example/plugin:1.0.0"} {
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

// authenticated serves an in-memory OCI registry that challenges requests
// without the fixture credentials and counts the requests that write.
func authenticated(writes *atomic.Int64) http.Handler {
	backend := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, _ := r.BasicAuth(); user != "fixture" || password != "synthetic-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes.Add(1)
		}
		backend.ServeHTTP(w, r)
	})
}
