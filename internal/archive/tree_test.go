package archive

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// posixNames are legal on macOS and Linux and unrepresentable on Windows.
var posixNames = []string{"Chasing Shadows Clap:Snare 01.loopdata", `1\16 Alternating Pan.pst`, "trailing "}

func TestPackCarriesPOSIXNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot hold these names")
	}
	root := t.TempDir()
	for _, name := range posixNames {
		if err := os.WriteFile(filepath.Join(root, name), []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := Pack(t.Context(), root, &output); err != nil {
		t.Fatal(err)
	}
	packed := map[string]bool{}
	reader := tar.NewReader(&output)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		packed[header.Name] = true
	}
	for _, name := range posixNames {
		if !packed[name] {
			t.Fatalf("%q missing from %v", name, packed)
		}
	}
}

// TestExtractReportsNamesThisHostCannotHold covers the one place the rule is
// host-specific: POSIX hosts write these names, Windows refuses them by name
// rather than writing something else.
func TestExtractReportsNamesThisHostCannotHold(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.tar")
	f, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(f)
	for _, name := range posixNames {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 7, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(w.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	err = Extract(t.Context(), input, out)
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "cannot be created on this host") {
			t.Fatalf("Windows accepted an unrepresentable name: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range posixNames {
		if _, err := os.Lstat(filepath.Join(out, name)); err != nil {
			t.Fatal(err)
		}
	}
}
