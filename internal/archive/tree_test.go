package archive

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPackPreservesConfinedSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "file"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"file-link": "nested/file", "dir-link": "nested"}
	for name, target := range links {
		if err := os.Symlink(filepath.FromSlash(target), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := Pack(t.Context(), root, &output); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&output)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if target, ok := links[header.Name]; ok {
			if header.Typeflag != tar.TypeSymlink || header.Linkname != target || header.Size != 0 {
				t.Fatalf("symlink changed: %+v", header)
			}
			delete(links, header.Name)
		}
	}
	if len(links) != 0 {
		t.Fatalf("missing symlinks: %v", links)
	}
}

func TestPackRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.FromSlash("../outside"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := Pack(t.Context(), root, io.Discard); err == nil {
		t.Fatal("accepted escaping symlink")
	}
}
