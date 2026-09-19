package windowssoftware

import (
	"bytes"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/plugin"
)

func TestIconExtractsTheRegisteredProductIcon(t *testing.T) {
	subject, err := Icon(t.Context(), plugin.Artifact{Path: "../msi/testdata/icon.msi", Format: "msi"})
	if err != nil || subject.Path != "" {
		t.Fatalf("subject %+v: %v", subject, err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(subject.Artwork))
	if err != nil || config.Width != 128 || config.Height != 128 {
		t.Fatalf("artwork %dx%d: %v", config.Width, config.Height, err)
	}
	// A setup directory names its installer through the entry point.
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../msi/testdata/icon.msi")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "bin", "setup.msi"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	staged, err := Icon(t.Context(), plugin.Artifact{Path: tree, Tree: true, EntryPoint: "bin/setup.msi"})
	if err != nil || !bytes.Equal(staged.Artwork, subject.Artwork) {
		t.Fatalf("setup directory: %v", err)
	}
	if _, err := Icon(t.Context(), plugin.Artifact{Path: "../msi/testdata/test.msi"}); !errors.Is(err, icon.ErrNoArtwork) {
		t.Fatalf("installer without ARPPRODUCTICON: %v", err)
	}
	script := filepath.Join(t.TempDir(), "setup.cmd")
	if err := os.WriteFile(script, []byte("@echo off\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Icon(t.Context(), plugin.Artifact{Path: script}); !errors.Is(err, icon.ErrNoArtwork) {
		t.Fatalf("script installer: %v", err)
	}
}
