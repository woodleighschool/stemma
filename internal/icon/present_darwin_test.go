package icon

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestGlassyPresentationDrawsBundlesAndArtwork(t *testing.T) {
	bundle, _, err := Present(t.Context(), Subject{Path: "/System/Applications/TextEdit.app"}, Options{Presentation: Auto})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(bundle); err != nil {
		t.Fatal(err)
	}
	// Artwork without a bundle, as Windows software provides, gets the same treatment.
	artwork := testPNG(t, 256, 5)
	data, presentation, err := Present(t.Context(), Subject{Artwork: artwork}, Options{Presentation: Glassy, Size: 256, Workspace: t.TempDir()})
	if err != nil || presentation != Glassy {
		t.Fatalf("glassy artwork: %s %v", presentation, err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 256 || config.Height != 256 || bytes.Equal(data, artwork) {
		t.Fatalf("glassy artwork was not drawn: %dx%d %v", config.Width, config.Height, err)
	}
	if _, _, err := Present(t.Context(), Subject{Path: t.TempDir()}, Options{Presentation: Glassy}); err == nil {
		t.Fatal("rendered a directory that is not a bundle")
	}
}

func TestGlassyMarkerAvoidsProhibitedBadge(t *testing.T) {
	artwork := Subject{Artwork: testPNG(t, 256, 180)}
	var rendered [][]byte
	for _, variant := range []string{"native", "marker", "missing"} {
		bundle, err := surrogate(artwork, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		executable := filepath.Join(bundle, "Contents/MacOS/application")
		switch variant {
		case "native":
			data, err := os.ReadFile("/usr/bin/true")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(executable, data, 0755); err != nil {
				t.Fatal(err)
			}
		case "missing":
			if err := os.Remove(executable); err != nil {
				t.Fatal(err)
			}
		}
		data, _, err := Present(t.Context(), Subject{Path: bundle}, Options{Presentation: Glassy, Size: 256})
		if err != nil {
			t.Fatal(err)
		}
		rendered = append(rendered, data)
	}
	native := decodePNG(t, rendered[0])
	marker := decodePNG(t, rendered[1])
	missing := decodePNG(t, rendered[2])
	difference := func(a, b []byte) float64 {
		var total int
		for i, v := range a {
			delta := int(v) - int(b[i])
			if delta < 0 {
				delta = -delta
			}
			total += delta
		}
		return float64(total) / float64(len(a))
	}
	if d := difference(native.Pix, marker.Pix); d > 1 {
		t.Fatalf("marker differs from native rendering by %.2f/channel", d)
	}
	if d := difference(marker.Pix, missing.Pix); d < 5 {
		t.Fatalf("missing executable did not produce the expected badge: %.2f/channel", d)
	}
}
