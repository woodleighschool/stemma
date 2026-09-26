package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/oras-project/oras-go/v3"
	"github.com/oras-project/oras-go/v3/content/memory"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
)

func TestLocalSnapshot(t *testing.T) {
	for _, tree := range []bool{false, true} {
		name := "file"
		if tree {
			name = "tree"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "run")
			if err := os.WriteFile(path, []byte("original executable"), 0o755); err != nil {
				t.Fatal(err)
			}
			declaration := config.Plugin{Path: "run", Trusted: true}
			if tree {
				declaration.Path = root
				declaration.Entrypoint = "run"
				path = filepath.Join(root, "helper.txt")
				if err := os.WriteFile(path, []byte("original helper"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			cache, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			store := New(cache, true)
			if _, _, err := store.Load(t.Context(), root, declaration, Entry{}, false); err == nil || !strings.Contains(err.Error(), "not locked") {
				t.Fatalf("unlocked local files loaded: %v", err)
			}
			bundle, entry, err := store.Load(t.Context(), root, declaration, Entry{}, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Load(t.Context(), root, declaration, entry, false); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(cache.Dir); err != nil {
				t.Fatal(err)
			}
			cache, err = cas.Open(cache.Dir)
			if err != nil {
				t.Fatal(err)
			}
			store = New(cache, true)
			recovered, locked, err := store.Load(t.Context(), root, declaration, entry, false)
			if err != nil || recovered.Manifest != bundle.Manifest || locked != entry {
				t.Fatalf("cold recovery changed plugin: %v", err)
			}
			if err := os.WriteFile(path, []byte("changed"), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Load(t.Context(), root, declaration, entry, false); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("locked load accepted changed local code: %v", err)
			}
			executable, err := store.Materialize(t.Context(), bundle, filepath.Join(t.TempDir(), "snapshot"))
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(executable)
			if err != nil || string(original) != "original executable" {
				t.Fatalf("snapshot used live code: %s %v", original, err)
			}
			if tree {
				helper, err := os.ReadFile(filepath.Join(filepath.Dir(executable), "helper.txt"))
				if err != nil || string(helper) != "original helper" {
					t.Fatalf("lost helper snapshot: %s %v", helper, err)
				}
			}
			changed, updated, err := store.Load(t.Context(), root, declaration, entry, true)
			if err != nil || changed.Manifest == bundle.Manifest || updated.Digest == entry.Digest {
				t.Fatalf("local change kept old identity: %v", err)
			}
		})
	}
}

func TestImageTagsLockTheirIndex(t *testing.T) {
	target := memory.New()
	first, _ := fixtureManifest(t, target, "linux", "amd64", fixtureArchive(t, "plugin", "first", false))
	firstIndex := fixtureIndex(t, target, first)
	second, _ := fixtureManifest(t, target, "linux", "amd64", fixtureArchive(t, "plugin", "second", false))
	secondIndex := fixtureIndex(t, target, second)
	const image = "registry.example/plugin:dev"
	if err := target.Tag(t.Context(), firstIndex, image); err != nil {
		t.Fatal(err)
	}
	cache, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cache, false)
	s.platform = ocispec.Platform{OS: "linux", Architecture: "amd64"}
	s.registry = func(string) (oras.ReadOnlyTarget, error) { return target, nil }
	declaration := config.Plugin{Image: image, Trusted: true}
	if _, _, err := s.Load(t.Context(), "", declaration, Entry{}, false); err == nil || !strings.Contains(err.Error(), "not locked") {
		t.Fatalf("unlocked tag loaded: %v", err)
	}
	bundle, entry, err := s.Load(t.Context(), "", declaration, Entry{}, true)
	if err != nil || bundle.Manifest != first.Digest.String() || entry.Digest != firstIndex.Digest.String() {
		t.Fatalf("resolved %+v to %+v: %v", entry, bundle, err)
	}
	if err := target.Tag(t.Context(), secondIndex, image); err != nil {
		t.Fatal(err)
	}
	if locked, _, err := s.Load(t.Context(), "", declaration, entry, false); err != nil || locked.Manifest != first.Digest.String() {
		t.Fatalf("locked load followed the moved tag: %+v %v", locked, err)
	}
	if settled, kept, err := s.Load(t.Context(), "", declaration, entry, true); err != nil || kept != entry || settled.Manifest != first.Digest.String() {
		t.Fatalf("resolving a locked declaration moved it: %+v %v", kept, err)
	}
	if _, refreshed, err := s.Load(t.Context(), "", declaration, Entry{}, true); err != nil || refreshed.Digest != secondIndex.Digest.String() {
		t.Fatalf("refresh kept the old index: %+v %v", refreshed, err)
	}
	other := config.Plugin{Image: "registry.example/plugin:main", Trusted: true}
	if _, _, err := s.Load(t.Context(), "", other, entry, false); err == nil || !strings.Contains(err.Error(), "not locked") {
		t.Fatalf("another tag used the old entry: %v", err)
	}
	pinned := config.Plugin{Image: "registry.example/plugin:dev@" + firstIndex.Digest.String(), Trusted: true}
	bundle, entry, err = s.Load(t.Context(), "", pinned, Entry{}, false)
	if err != nil || bundle.Manifest != first.Digest.String() || entry != (Entry{}) {
		t.Fatalf("declared digest needed a lock entry: %+v %+v %v", bundle, entry, err)
	}
}
