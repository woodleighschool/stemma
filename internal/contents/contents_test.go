package contents

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
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
				source, err := Open(t.Context(), input, t.TempDir())
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

func TestOpaqueContainerNames(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "command"), []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	var tar bytes.Buffer
	if err := archive.Pack(t.Context(), root, &tar); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(tar.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"zip", "tar", "tar.gz", "dmg"} {
		t.Run(format, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "download.php")
			switch format {
			case "zip":
				testarchive.Zip(t, name, root)
			case "dmg":
				testdiskimage.Write(t, name, root)
			default:
				data := tar.Bytes()
				if format == "tar.gz" {
					data = compressed.Bytes()
				}
				if err := os.WriteFile(name, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			source, err := Open(t.Context(), plugin.Artifact{Path: name, Filename: "download.php"}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			if !source.Traversable() {
				t.Fatal("container treated as scalar")
			}
			whole, err := source.At(t.Context(), "")
			if err != nil || whole.Local != name {
				t.Fatalf("lost original input: %+v %v", whole, err)
			}
			node, err := source.At(t.Context(), "command")
			if err != nil {
				t.Fatal(err)
			}
			data, err := fs.ReadFile(node.FS, node.Path)
			if err != nil || string(data) != "payload" {
				t.Fatalf("content = %q: %v", data, err)
			}
		})
	}
}

func TestOpaquePackageRemainsScalar(t *testing.T) {
	data, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "latest")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := Open(t.Context(), plugin.Artifact{Path: name, Filename: "latest"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if source.Traversable() {
		t.Fatal("PKG exposed as an archive")
	}
	if _, err := source.At(t.Context(), "Payload"); err == nil {
		t.Fatal("traversed PKG")
	}
}
