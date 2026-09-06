package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/icon"
)

func TestIconCommandRetainsSuppliedPNG(t *testing.T) {
	var input bytes.Buffer
	if err := png.Encode(&input, image.NewNRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.png")
	if err := os.WriteFile(source, input.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "retained.png")
	var report bytes.Buffer
	cmd := iconCommand(&report)
	cmd.SetArgs([]string{source, "--out", output})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	var result icon.Result
	if err := json.Unmarshal(report.Bytes(), &result); err != nil || result.Path != output || result.Renderer != "supplied" {
		t.Fatalf("invalid icon result: %s: %v", report.Bytes(), err)
	}
	data, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(data, input.Bytes()) {
		t.Fatalf("CLI changed supplied PNG: %v", err)
	}
}
