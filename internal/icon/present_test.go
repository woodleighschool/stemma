package icon

import (
	"bytes"
	"errors"
	"testing"
)

func TestRawPresentationWritesArtworkUnchanged(t *testing.T) {
	artwork := testPNG(t, 128, 3)
	data, presentation, err := Present(t.Context(), Subject{Artwork: artwork}, Options{Presentation: Raw})
	if err != nil || presentation != Raw || !bytes.Equal(data, artwork) {
		t.Fatalf("raw presentation: %s %d bytes %v", presentation, len(data), err)
	}
	if _, _, err := Present(t.Context(), Subject{Path: t.TempDir()}, Options{Presentation: Raw}); !errors.Is(err, ErrNoArtwork) {
		t.Fatalf("raw presentation without artwork: %v", err)
	}
	if _, err := ParsePresentation("shiny"); err == nil {
		t.Fatal("accepted an unknown presentation")
	}
	if _, err := Presentation("shiny").Resolve(); err == nil {
		t.Fatal("resolved an unknown presentation")
	}
	if resolved, err := Auto.Resolve(); err != nil || resolved != hostPresentation {
		t.Fatalf("auto resolved to %q: %v", resolved, err)
	}
}
