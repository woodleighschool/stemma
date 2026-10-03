package icon

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"os"
	"testing"
)

func TestFromICNSPicksTheLargestEntryWithinBounds(t *testing.T) {
	small, medium, large := testPNG(t, 16, 1), testPNG(t, 128, 2), testPNG(t, 256, 3)
	data, err := FromICNS(testICNS(map[string][]byte{"ic07": medium, "ic08": large, "icp4": small, "info": []byte("junk")}))
	if err != nil || !bytes.Equal(data, large) {
		t.Fatalf("picked %d bytes: %v", len(data), err)
	}
	if data, err := FromICNS(testICNS(map[string][]byte{"icp4": small, "it32": []byte("broken")})); err != nil || Validate(data) != nil {
		t.Fatalf("usable small frame: %v", err)
	}
	if _, err := FromICNS([]byte("icns\x00\x00\x00\x10junk")); err == nil || errors.Is(err, ErrNoArtwork) {
		t.Fatalf("truncated file: %v", err)
	}
}

func TestFromICOConvertsBitmapFramesAndKeepsPNGFrames(t *testing.T) {
	medium := testImage(128, 7)
	large := testImage(256, 9)
	data, err := FromICO(testICO(testPNG(t, 128, 7), testDIB(large, 32, true)))
	if err != nil {
		t.Fatal(err)
	}
	assertImage(t, data, large)
	kept, err := FromICO(testICO(testDIB(testImage(16, 1), 32, true), testPNG(t, 128, 7)))
	if err != nil || !bytes.Equal(kept, testPNG(t, 128, 7)) {
		t.Fatalf("PNG frame was re-encoded: %v", err)
	}
	for name, frame := range map[string][]byte{
		"32-bit with mask": testDIB(medium, 32, false),
		"24-bit with mask": testDIB(medium, 24, false),
		"8-bit palette":    testDIB(medium, 8, false),
	} {
		data, err := FromICO(testICO(frame))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertImage(t, data, medium)
	}
	if data, err := FromICO(testICO(testDIB(testImage(64, 1), 32, true))); err != nil || Validate(data) != nil {
		t.Fatalf("small native icon: %v", err)
	}
	if _, err := FromICO([]byte("not an icon")); err == nil {
		t.Fatal("accepted a file without an ICO directory")
	}
}

func TestFromICNSReadsNativeEncodings(t *testing.T) {
	// Uncompressed planar RGB with an independent alpha mask.
	const pixels = 128 * 128
	rgb := make([]byte, 4+3*pixels)
	copy(rgb[4:], bytes.Repeat([]byte{200}, pixels))
	copy(rgb[4+pixels:], bytes.Repeat([]byte{80}, pixels))
	copy(rgb[4+2*pixels:], bytes.Repeat([]byte{30}, pixels))
	mask := bytes.Repeat([]byte{128}, pixels)
	data, err := FromICNS(testICNS(map[string][]byte{"it32": rgb, "t8mk": mask}))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodePNG(t, data).NRGBAAt(40, 40); got != (color.NRGBA{R: 200, G: 80, B: 30, A: 128}) {
		t.Fatalf("planar colour and mask: %v", got)
	}
	// This JP2 was encoded by macOS ImageIO, independently of the decoder.
	jp2, err := os.ReadFile("testdata/quadrants.jp2")
	if err != nil {
		t.Fatal(err)
	}
	data, err = FromICNS(testICNS(map[string][]byte{"ic07": jp2}))
	if err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, data)
	for _, sample := range []struct {
		x, y int
		want color.NRGBA
	}{
		{32, 32, color.NRGBA{}},
		{32, 96, color.NRGBA{R: 240, G: 32, B: 64, A: 255}},
		{96, 96, color.NRGBA{R: 32, G: 224, B: 96, A: 255}},
	} {
		got, want := img.NRGBAAt(sample.x, sample.y), sample.want
		near := func(a, b uint8) bool { return int(a)-int(b) <= 2 && int(b)-int(a) <= 2 }
		if !near(got.R, want.R) || !near(got.G, want.G) || !near(got.B, want.B) || got.A != want.A {
			t.Fatalf("JP2 pixel %d,%d: %v, want approximately %v", sample.x, sample.y, got, want)
		}
	}
}

func TestBrokenArtworkIsNotReportedAsAbsent(t *testing.T) {
	for name, decode := range map[string]func() ([]byte, error){
		"icns": func() ([]byte, error) { return FromICNS(testICNS(map[string][]byte{"ic09": []byte("broken")})) },
		"ico":  func() ([]byte, error) { return FromICO(testICO([]byte("broken"))) },
		"pe":   func() ([]byte, error) { return FromPE(bytes.NewReader(testPE(0, []byte("broken")))) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(); err == nil || errors.Is(err, ErrNoArtwork) {
				t.Fatalf("broken artwork: %v", err)
			}
		})
	}
}

func TestFromPEReadsTheFirstIconGroup(t *testing.T) {
	large := testImage(256, 5)
	for _, offset := range []uint32{0, 32} {
		data, err := FromPE(bytes.NewReader(testPE(offset, testPNG(t, 128, 4), testDIB(large, 32, true))))
		if err != nil {
			t.Fatalf("resource directory at offset %d: %v", offset, err)
		}
		assertImage(t, data, large)
	}
	if _, err := FromPE(bytes.NewReader(testPE(0))); !errors.Is(err, ErrNoArtwork) {
		t.Fatalf("executable without resources: %v", err)
	}
	if _, err := FromPE(bytes.NewReader([]byte("MZ garbage"))); err == nil || errors.Is(err, ErrNoArtwork) {
		t.Fatalf("garbage: %v", err)
	}
}

func assertImage(t *testing.T, data []byte, want *image.NRGBA) {
	t.Helper()
	if err := Validate(data); err != nil {
		t.Fatal(err)
	}
	got := decodePNG(t, data)
	if got.Bounds() != want.Bounds() {
		t.Fatalf("decoded %v, want %v", got.Bounds(), want.Bounds())
	}
	for y := range want.Bounds().Dy() {
		for x := range want.Bounds().Dx() {
			if g, w := got.NRGBAAt(x, y), want.NRGBAAt(x, y); g != w {
				t.Fatalf("pixel %d,%d is %v, want %v", x, y, g, w)
			}
		}
	}
}
