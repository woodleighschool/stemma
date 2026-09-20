package source

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func TestLocalIncludesSnapshotOnlyMatchedInputs(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "software", "Branding")
	writeInput(t, filepath.Join(base, "Payload", "Branding.txt"), "branding", 0o640)
	writeInput(t, filepath.Join(base, "Scripts", "postinstall"), "#!/bin/sh\nexit 0\n", 0o755)
	script, err := os.Stat(filepath.Join(base, "Scripts", "postinstall"))
	if err != nil {
		t.Fatal(err)
	}
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
	s := plugin.Input{Resolver: "local", Base: "software/Branding", Config: map[string]any{"include": []string{"Payload/**", "Scripts/**"}}}
	first, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	filename, err := store.Path(first.Content.Artifact)
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
	if len(entries) != 5 || entries["Scripts/postinstall"].Mode != int64(script.Mode().Perm()) || entries["Payload/current"].Linkname != "Branding.txt" {
		t.Fatalf("snapshot selection, modes, or symlinks changed: %#v", entries)
	}
	writeInput(t, filepath.Join(base, "adjacent.txt"), "new unrelated content", 0o600)
	writeInput(t, filepath.Join(base, "stemma.yaml"), "new configuration", 0o600)
	second, err := m.Resolve(t.Context(), s)
	if err != nil || second.Content.Artifact != first.Content.Artifact {
		t.Fatalf("adjacent inputs affected identity: %#v %v", second, err)
	}
	writeInput(t, filepath.Join(base, "Scripts", "postinstall"), "#!/bin/sh\necho changed\n", 0o755)
	if _, err := m.FetchLocked(t.Context(), s, first); err == nil {
		t.Fatal("warm CAS hid a local script change")
	}
	third, err := m.Resolve(t.Context(), s)
	if err != nil || third.Content.Artifact == first.Content.Artifact {
		t.Fatalf("matched script did not affect identity: %#v %v", third, err)
	}
}

func TestFamilyRelativeInputDeclarations(t *testing.T) {
	root := t.TempDir()
	writeInput(t, filepath.Join(root, "software", "Shared", "script"), "shared script", 0o755)
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, root, false)
	input := plugin.Input{Resolver: "file", Base: "software/Branding", Config: map[string]any{"path": "../Shared/script"}}
	entry, err := m.Resolve(t.Context(), input)
	if err != nil || entry.Content.Filename != "script" {
		t.Fatalf("family-relative input: %v", err)
	}
	equivalent := plugin.Input{Resolver: "file", Base: "software/Shared", Config: map[string]any{"path": "script"}}
	if _, hash, err := m.Declaration(equivalent); err != nil || hash != entry.Declaration {
		t.Fatalf("same project input has a different declaration: %v", err)
	}
	for _, name := range []string{`Shared\script`, "Shared:script", "Shared/\x00"} {
		input.Config["path"] = name
		if _, _, err := m.Declaration(input); err == nil {
			t.Fatalf("accepted unusable family-relative input %q", name)
		}
	}
	remote := plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://example.invalid/app.pkg"}}
	_, first, err := m.Declaration(remote)
	if err != nil {
		t.Fatal(err)
	}
	remote.Base = "software/Branding"
	if _, second, err := m.Declaration(remote); err != nil || second != first {
		t.Fatalf("family location affected remote input declaration: %v", err)
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
		if _, err := m.Resolve(t.Context(), plugin.Input{Resolver: "local", Config: map[string]any{"include": []string{pattern}}}); err == nil {
			t.Fatalf("accepted pattern %q", pattern)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), plugin.Input{Resolver: "local", Config: map[string]any{"include": []string{"outside/**"}}}); err == nil {
		t.Fatal("traversed symlink outside local scope")
	}
	if err := os.Symlink("Payload", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), plugin.Input{Resolver: "local", Config: map[string]any{"include": []string{"alias/**"}}}); err == nil {
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

func TestFileInputsUseFilesystemPathSemantics(t *testing.T) {
	host := t.TempDir()
	root := filepath.Join(host, "catalog")
	outside := filepath.Join(host, "Applications")
	writeInput(t, filepath.Join(outside, "Vendor.pkg"), "vendor installer", 0o644)
	writeInput(t, filepath.Join(outside, "App.app", "Contents", "Info.plist"), "bundle", 0o644)
	writeInput(t, filepath.Join(root, "software", "Vendor", "stemma.yaml"), "configuration", 0o644)
	if err := os.Symlink(filepath.Join(outside, "Vendor.pkg"), filepath.Join(root, "Vendor.link")); err != nil {
		t.Fatal(err)
	}
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, root, false)

	absolute, err := m.Resolve(t.Context(), plugin.Input{Resolver: "file", Base: "software/Vendor", Config: map[string]any{"path": filepath.Join(outside, "Vendor.pkg")}})
	if err != nil || absolute.Content.Filename != "Vendor.pkg" || absolute.Content.Tree {
		t.Fatalf("absolute file input: %#v %v", absolute.Content, err)
	}
	bundle, err := m.Resolve(t.Context(), plugin.Input{Resolver: "file", Config: map[string]any{"path": filepath.Join(outside, "App.app")}})
	if err != nil || bundle.Content.Filename != "App.app" || !bundle.Content.Tree {
		t.Fatalf("absolute directory input: %#v %v", bundle.Content, err)
	}

	escaping, err := m.Resolve(t.Context(), plugin.Input{Resolver: "file", Base: "software/Vendor", Config: map[string]any{"path": "../../../Applications/Vendor.pkg"}})
	if err != nil || escaping.Content.Artifact != absolute.Content.Artifact {
		t.Fatalf("relative input above the project: %#v %v", escaping.Content, err)
	}
	if escaping.Declaration == absolute.Declaration {
		t.Fatal("project-relative and host paths share a declaration")
	}

	link, err := m.Resolve(t.Context(), plugin.Input{Resolver: "file", Config: map[string]any{"path": "Vendor.link"}})
	if err != nil || link.Content.Artifact != absolute.Content.Artifact {
		t.Fatalf("symlinked input: %#v %v", link.Content, err)
	}
}
