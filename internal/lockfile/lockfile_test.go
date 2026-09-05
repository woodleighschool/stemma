package lockfile

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"go.yaml.in/yaml/v4"
)

func TestLockedColdWarmOfflineAndRefresh(t *testing.T) {
	var requests atomic.Int32
	var payload atomic.Value
	payload.Store("original installer")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(payload.Load().(string)))
	}))
	defer server.Close()
	root := t.TempDir()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := source.New(store, root, false)
	p := config.Project{Version: 1, Project: "test", Recipes: map[string]config.Recipe{"app": {Source: config.Source{Type: "http", URL: server.URL + "/app.pkg"}}}}
	if _, err := Prepare(t.Context(), p, m, Options{Frozen: true}); err == nil {
		t.Fatal("frozen run accepted missing lockfile")
	}
	first, err := Prepare(t.Context(), p, m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || requests.Load() != 1 {
		t.Fatalf("first run %#v requests=%d", first, requests.Load())
	}
	if first.File.Recipes["app"].ResolvedAt.IsZero() {
		t.Fatal("source lock has no stable package timestamp")
	}
	entry := first.File.Recipes["app"]
	entry.ResolvedAt = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	first.File.Recipes["app"] = entry
	data, err := yaml.Marshal(first.File)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stemma.lock.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(t.Context(), p, m, Options{Frozen: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed || !second.CacheHits["app"] || requests.Load() != 1 {
		t.Fatal("warm run downloaded or changed lock")
	}
	refreshed, err := Prepare(t.Context(), p, m, Options{Refresh: true})
	if err != nil || refreshed.Changed || refreshed.File.Recipes["app"].ResolvedAt != entry.ResolvedAt {
		t.Fatalf("unchanged refresh replaced source timestamp: %+v, %v", refreshed, err)
	}
	m.Offline = true
	if _, err := Prepare(t.Context(), p, m, Options{Frozen: true, Offline: true}); err != nil {
		t.Fatal(err)
	}
	m.Offline = false
	locked, err := os.ReadFile(filepath.Join(root, "stemma.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	payload.Store("replaced installer")
	object, err := store.Path(first.File.Recipes["app"].Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(t.Context(), p, m, Options{Frozen: true}); err == nil {
		t.Fatal("accepted substituted upstream bytes")
	}
	after, err := os.ReadFile(filepath.Join(root, "stemma.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(locked) {
		t.Fatal("failed run rewrote lockfile")
	}
	updated, err := Prepare(t.Context(), p, m, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.File.Recipes["app"].Artifact == first.File.Recipes["app"].Artifact {
		t.Fatal("refresh did not digest new bytes")
	}
	if !updated.File.Recipes["app"].ResolvedAt.After(entry.ResolvedAt) {
		t.Fatal("changed bytes retained old source timestamp")
	}
	if _, err := Prepare(t.Context(), p, m, Options{Ignore: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(t.Context(), p, m, Options{Ignore: true, Frozen: true}); err == nil {
		t.Fatal("accepted contradictory flags")
	}
}

func TestLocalChangesRehashWarmLocks(t *testing.T) {
	for _, kind := range []string{"file", "local"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			input := filepath.Join(root, "postinstall")
			if err := os.WriteFile(input, []byte("old script"), 0o755); err != nil {
				t.Fatal(err)
			}
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := source.New(store, root, false)
			s := config.Source{Type: kind, Path: "postinstall"}
			if kind == "local" {
				s.Path, s.Include = "", []string{"postinstall"}
			}
			p := config.Project{Version: 1, Project: "test", Recipes: map[string]config.Recipe{"app": {Source: s}}}
			first, err := Prepare(t.Context(), p, m, Options{})
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(root, "stemma.lock.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
			if err := os.Chtimes(input, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare(t.Context(), p, m, Options{Frozen: true}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(input, []byte("changed script"), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare(t.Context(), p, m, Options{Frozen: true}); err == nil {
				t.Fatal("frozen lock accepted changed local bytes already present in CAS")
			}
			after, err := os.ReadFile(filepath.Join(root, "stemma.lock.yaml"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed frozen run modified lockfile")
			}
			m.Offline = true
			if _, err := Prepare(t.Context(), p, m, Options{Offline: true}); err == nil {
				t.Fatal("offline lock accepted changed local bytes")
			}
			m.Offline = false
			updated, err := Prepare(t.Context(), p, m, Options{})
			if err != nil || !updated.Changed || updated.File.Recipes["app"].Artifact == first.File.Recipes["app"].Artifact {
				t.Fatalf("unfrozen run did not acquire local changes: %#v, %v", updated, err)
			}
			warm, err := Prepare(t.Context(), p, m, Options{Frozen: true})
			if err != nil || warm.Changed || !warm.CacheHits["app"] {
				t.Fatalf("unchanged local run did not converge: %#v, %v", warm, err)
			}
		})
	}
}
