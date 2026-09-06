package icon

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestSuppliedPNGOnlyChangesOnExplicitRefresh(t *testing.T) {
	source := filepath.Join(t.TempDir(), "supplied.png")
	first := fixturePNG(t, color.NRGBA{R: 255, A: 255})
	write(t, source, first)
	output := filepath.Join(t.TempDir(), "retained.png")
	initial, err := Export(t.Context(), source, output, Options{})
	if err != nil || initial.Renderer != "supplied" {
		t.Fatalf("supplied image: %+v: %v", initial, err)
	}
	second := fixturePNG(t, color.NRGBA{B: 255, A: 255})
	write(t, source, second)
	reused, err := Export(t.Context(), source, output, Options{})
	if err != nil || !reused.Reused || reused.SHA256 != initial.SHA256 {
		t.Fatalf("source changes replaced retained image: %+v: %v", reused, err)
	}
	refreshed, err := Export(t.Context(), source, output, Options{Refresh: true, Size: 16})
	if err != nil || refreshed.Reused || refreshed.SHA256 == initial.SHA256 || refreshed.Width != 64 {
		t.Fatalf("explicit refresh did not retain supplied image: %+v: %v", refreshed, err)
	}
	data, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(data, second) {
		t.Fatalf("supplied PNG was regenerated: %v", err)
	}
}

func TestFailedRefreshKeepsRetainedImage(t *testing.T) {
	output := filepath.Join(t.TempDir(), "retained.png")
	retained := fixturePNG(t, color.NRGBA{G: 255, A: 255})
	write(t, output, retained)
	reused, err := Export(t.Context(), "missing.app", output, Options{})
	if err != nil || !reused.Reused {
		t.Fatalf("retained image required rendering: %+v, %v", reused, err)
	}
	if _, err := Export(t.Context(), "missing.app", output, Options{Refresh: true}); err == nil {
		t.Fatal("missing app rendered")
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, retained) {
		t.Fatalf("failed refresh changed retained icon: %v", err)
	}
}

func TestCancelledExportDoesNotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	output := filepath.Join(t.TempDir(), "icon.png")
	if _, err := Export(ctx, "missing.app", output, Options{}); err == nil {
		t.Fatal("cancelled export succeeded")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("cancelled export wrote an image")
	}
}

func fixturePNG(t *testing.T, fill color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.SetNRGBA(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
