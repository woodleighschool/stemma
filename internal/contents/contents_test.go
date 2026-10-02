package contents

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/plugin"
)

func TestResolverContentRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tool", "1.2", "libexec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tool", "1.2", "libexec", "run"), []byte("command"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside"), []byte("unselected"), 0o644); err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(t.TempDir(), "tool.zip")
	testarchive.Zip(t, packed, root)
	if err := os.Symlink("tool/1.2", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []bool{false, true} {
		for _, selection := range []string{"tool/1.2", "missing", "../escape", "alias", "tool/1.2/libexec/run"} {
			t.Run(selection+map[bool]string{false: "/archive", true: "/tree"}[tree], func(t *testing.T) {
				filename := packed
				if tree {
					filename = root
				}
				data, _ := json.Marshal(map[string]any{"path": filename, "filename": "tool.zip", "tree": tree, "content_root": selection})
				var input plugin.Artifact
				if err := json.Unmarshal(data, &input); err != nil {
					t.Fatal(err)
				}
				source, err := Open(input, t.TempDir())
				if err != nil {
					if selection == "tool/1.2" {
						t.Fatal(err)
					}
					return
				}
				defer func() { _ = source.Close() }()
				node, err := source.At(t.Context(), "")
				if selection != "tool/1.2" {
					if err == nil {
						t.Fatal("invalid content root accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				info, err := node.Stat()
				if err != nil || !info.IsDir() {
					t.Fatalf("input does not select resolved directory: %v", err)
				}
				command, err := source.At(t.Context(), "libexec/run")
				if err != nil {
					t.Fatal(err)
				}
				data, err = fs.ReadFile(command.FS, command.Path)
				if err != nil || string(data) != "command" {
					t.Fatalf("wrong resolved input content: %s %v", data, err)
				}
				if _, err := source.At(t.Context(), "outside"); err == nil {
					t.Fatal("selected sibling outside resolved root")
				}
			})
		}
	}
}
