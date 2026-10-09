package contents

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/plugin"
)

func TestPackedTreeSelectionsAndMaterialization(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "selected"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "selected", "Installer.pkg"), []byte("package"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "outside"), []byte("sibling"), 0o644); err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	if err := archive.Pack(t.Context(), directory, &packed); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(t.TempDir(), "tree.tar")
	if err := os.WriteFile(lease, packed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	input := plugin.Artifact{Path: lease, Filename: "Vendor", Tree: true, Mode: 0o750, Encoding: "tar", ContentRoot: "selected"}
	source, err := Open(t.Context(), input, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	node, err := source.At(t.Context(), "Installer.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if node.Local != "" {
		t.Fatal("packed selection expanded eagerly")
	}
	if _, err := source.At(t.Context(), "../outside"); err == nil {
		t.Fatal("escaped selection")
	}
	if _, err := source.At(t.Context(), "outside"); err == nil {
		t.Fatal("selected sibling")
	}
	destination := t.TempDir()
	name, err := node.Materialize(t.Context(), destination)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name)
	if err != nil || string(data) != "package" {
		t.Fatalf("materialized %q: %v", data, err)
	}
	if _, err := node.Materialize(t.Context(), destination); err == nil {
		t.Fatal("overwrote existing selection")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	input.ContentRoot = ""
	source, err = Open(t.Context(), input, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	whole, err := source.At(t.Context(), "")
	if err != nil || whole.Path != "Vendor" {
		t.Fatalf("logical root: %+v %v", whole, err)
	}
	info, err := whole.Stat()
	if err != nil || info.Mode() != fs.ModeDir|0o750 {
		t.Fatalf("logical root mode: %v %v", info, err)
	}
	if err := os.WriteFile(lease, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err == nil {
		t.Fatal("missed mutation of held tree lease")
	}
}
