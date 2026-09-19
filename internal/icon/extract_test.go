package icon

import (
	"bytes"
	"errors"
	"image"
	"testing"
)

func TestFromICNSPicksTheLargestEntryWithinBounds(t *testing.T) {
	small, medium, large := testPNG(t, 16, 1), testPNG(t, 128, 2), testPNG(t, 256, 3)
	data, err := FromICNS(testICNS(map[string][]byte{"ic07": medium, "ic08": large, "icp4": small, "info": []byte("junk")}))
	if err != nil || !bytes.Equal(data, large) {
		t.Fatalf("picked %d bytes: %v", len(data), err)
	}
	if _, err := FromICNS(testICNS(map[string][]byte{"icp4": small, "it32": []byte("legacy")})); !errors.Is(err, ErrNoArtwork) {
		t.Fatalf("legacy-only file: %v", err)
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
	if _, err := FromICO(testICO(testDIB(testImage(64, 1), 32, true))); !errors.Is(err, ErrNoArtwork) {
		t.Fatalf("frames below the minimum edge: %v", err)
	}
	if _, err := FromICO([]byte("not an icon")); err == nil {
		t.Fatal("accepted a file without an ICO directory")
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
