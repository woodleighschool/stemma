package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
)

func TestExportPreservesArtifactAndSurvivesPruning(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "payload")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "run"), []byte("synthetic executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink("run", filepath.Join(source, "current")) == nil
	for _, tree := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "tree"}[tree], func(t *testing.T) {
			input := source
			if !tree {
				input = filepath.Join(source, "run")
			}
			ref, err := importPath(t.Context(), store, input, tree, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			prepared := Prepared{Payload: ref, Filename: "payload", Mode: 0o755, Tree: tree}
			target := filepath.Join(t.TempDir(), "export")
			path, err := expose(t.Context(), store, prepared, t.TempDir(), target)
			if err != nil || path != target {
				t.Fatalf("export=%s / %v", path, err)
			}
			if copied, err := importPath(t.Context(), store, path, tree, t.TempDir()); err != nil || copied != ref {
				t.Fatalf("export changed artifact: %+v / %+v / %v", copied, ref, err)
			}
			if _, err := expose(t.Context(), store, prepared, t.TempDir(), target); err == nil {
				t.Fatal("overwrote export")
			}
			if _, err := expose(t.Context(), store, prepared, t.TempDir(), filepath.Join(store.Dir, "export")); err == nil || !strings.Contains(err.Error(), "outside") {
				t.Fatalf("accepted cache export: %v", err)
			}
			if _, err := store.Prune(t.Context(), cas.Policy{}, cas.PruneOptions{All: true}); err != nil {
				t.Fatal(err)
			}
			file := target
			if tree {
				file = filepath.Join(target, "run")
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "synthetic executable" {
				t.Fatalf("prune changed export: %s / %v", data, err)
			}
			if tree && linked {
				if target, err := os.Readlink(filepath.Join(target, "current")); err != nil || target != "run" {
					t.Fatalf("export changed symlink: %s / %v", target, err)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "export")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cache export was created")
	}
}
