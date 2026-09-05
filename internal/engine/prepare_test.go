package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
)

func TestTreeSelectionAndCachedVersionOverride(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "inputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.exe", "second.exe"} {
		if err := os.WriteFile(filepath.Join(root, "inputs", name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := config.Source{Type: "file", Path: "inputs", Version: "1.0"}
	entry, err := source.New(store, root, false).Resolve(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	recipe := config.Recipe{Source: src, Select: "second.exe"}
	first, err := prepare(t.Context(), store, entry, recipe, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(first.Path)
	if err != nil || string(data) != "second.exe" || first.Tree || first.Version != "1.0" {
		t.Fatalf("selected preparation: %+v, data=%q, err=%v", first, data, err)
	}
	entry.Version = "2.0"
	second, err := prepare(t.Context(), store, entry, recipe, t.TempDir())
	if err != nil || !second.Cached || second.Version != "2.0" || second.Payload != first.Payload {
		t.Fatalf("version override rebuilt content or was stale: %+v, err=%v", second, err)
	}
	entry.Version = ""
	third, err := prepare(t.Context(), store, entry, recipe, t.TempDir())
	if err != nil || !third.Cached || third.Version != "" {
		t.Fatalf("removed version override persisted: %+v, err=%v", third, err)
	}
}
