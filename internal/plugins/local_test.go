package plugins

import (
	"os"
	"path/filepath"
	"testing"

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
			bundle, entry, err := store.Load(t.Context(), root, declaration, Entry{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Load(t.Context(), root, declaration, entry, true); err != nil {
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
			recovered, locked, err := store.Load(t.Context(), root, declaration, entry, true)
			if err != nil || recovered.Manifest != bundle.Manifest || !locked.Local.Equal(*entry.Local) {
				t.Fatalf("cold recovery changed plugin: %v", err)
			}
			if err := os.WriteFile(path, []byte("changed"), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Load(t.Context(), root, declaration, entry, true); err == nil {
				t.Fatal("frozen load accepted changed local code")
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
			changed, _, err := store.Load(t.Context(), root, declaration, entry, false)
			if err != nil || changed.Manifest == bundle.Manifest {
				t.Fatalf("local change kept old identity: %v", err)
			}
		})
	}
}
