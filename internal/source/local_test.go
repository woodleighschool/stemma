package source

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
)

func TestLocalIncludesSnapshotOnlyMatchedInputs(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "software", "Branding")
	writeInput(t, filepath.Join(base, "Payload", "Branding.txt"), "branding", 0o640)
	writeInput(t, filepath.Join(base, "Scripts", "postinstall"), "#!/bin/sh\nexit 0\n", 0o755)
	writeInput(t, filepath.Join(base, "stemma.yaml"), "configuration", 0o600)
	writeInput(t, filepath.Join(base, "adjacent.txt"), "unrelated", 0o600)
	if err := os.Symlink("Branding.txt", filepath.Join(base, "Payload", "current")); err != nil {
		t.Fatal(err)
	}
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, root, false)
	s := config.Source{Type: "local", Base: "software/Branding", Include: []string{"Payload/**", "Scripts/**"}}
	first, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	filename, err := store.Path(first.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	reader := tar.NewReader(f)
	entries := map[string]*tar.Header{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = header
	}
	if len(entries) != 5 || entries["Scripts/postinstall"].Mode != 0o755 || entries["Payload/current"].Linkname != "Branding.txt" {
		t.Fatalf("snapshot selection, modes, or symlinks changed: %#v", entries)
	}
	writeInput(t, filepath.Join(base, "adjacent.txt"), "new unrelated content", 0o600)
	writeInput(t, filepath.Join(base, "stemma.yaml"), "new configuration", 0o600)
	second, err := m.Resolve(t.Context(), s)
	if err != nil || second.Artifact != first.Artifact {
		t.Fatalf("adjacent inputs affected identity: %#v %v", second, err)
	}
	writeInput(t, filepath.Join(base, "Scripts", "postinstall"), "#!/bin/sh\necho changed\n", 0o755)
	if _, err := m.Acquire(t.Context(), s, first); err == nil {
		t.Fatal("warm CAS hid a local script change")
	}
	third, err := m.Resolve(t.Context(), s)
	if err != nil || third.Artifact == first.Artifact {
		t.Fatalf("matched script did not affect identity: %#v %v", third, err)
	}
}

func TestLocalIncludesRejectMissingOrEscapingInputs(t *testing.T) {
	root := t.TempDir()
	writeInput(t, filepath.Join(root, "Payload", "file"), "payload", 0o600)
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, root, false)
	for _, pattern := range []string{"missing/**", "../outside", "/absolute", "Payload/["} {
		if _, err := m.Resolve(t.Context(), config.Source{Type: "local", Include: []string{pattern}}); err == nil {
			t.Fatalf("accepted pattern %q", pattern)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), config.Source{Type: "local", Include: []string{"outside/**"}}); err == nil {
		t.Fatal("traversed symlink outside local scope")
	}
	if err := os.Symlink("Payload", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), config.Source{Type: "local", Include: []string{"alias/**"}}); err == nil {
		t.Fatal("silently flattened an included symlink parent")
	}
}

func writeInput(t *testing.T, filename, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
