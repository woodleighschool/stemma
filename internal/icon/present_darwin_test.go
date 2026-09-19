package icon

import (
	"bytes"
	"image/png"
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
