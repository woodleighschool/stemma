package icon

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"testing"
)

func TestRasterArtworkFitsPublicationBounds(t *testing.T) {
	for _, size := range []image.Point{{32, 32}, {200, 100}, {2048, 2048}} {
		t.Run(size.String(), func(t *testing.T) {
			img := image.NewNRGBA(image.Rectangle{Max: size})
			for y := range size.Y {
				for x := range size.X {
					img.SetNRGBA(x, y, color.NRGBA{R: 240, A: 255})
				}
			}
			var encoded bytes.Buffer
			if err := jpeg.Encode(&encoded, img, nil); err != nil {
				t.Fatal(err)
			}
			data, err := FromImage(encoded.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(data); err != nil {
				t.Fatal(err)
			}
			if size.X != size.Y && decodePNG(t, data).NRGBAAt(0, 0).A != 0 {
				t.Fatal("rectangular artwork was not padded with transparency")
			}
		})
	}
}

func TestDetailedArtworkFitsPublicationByteLimit(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	random := rand.NewChaCha8([32]byte{1})
	if _, err := random.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	data, err := FromImage(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(data); err != nil {
		t.Fatal(err)
	}
}

func TestArtworkRejectsOversizedAndUnknownImages(t *testing.T) {
	if _, err := FromImage(encodePNG(t, image.NewNRGBA(image.Rect(0, 0, maxSourceEdge+1, 1)))); err == nil {
		t.Fatal("oversized artwork accepted")
	}
	if _, err := FromImage([]byte("unknown artwork")); !errors.Is(err, ErrUnsupportedArtwork) {
		t.Fatalf("unknown artwork: %v", err)
	}
}
