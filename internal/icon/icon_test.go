package icon

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func encode(t *testing.T, width, height int) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestValidateBoundsTheCanonicalAsset(t *testing.T) {
	for _, test := range []struct {
		name          string
		width, height int
		want          bool
	}{
		{"canonical", Size, Size, true}, {"smallest", minEdge, minEdge, true}, {"largest", maxEdge, maxEdge, true},
		{"tiny", 64, 64, false}, {"huge", 2048, 2048, false}, {"wide", 512, 256, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate(encode(t, test.width, test.height)); (err == nil) != test.want {
				t.Fatalf("Validate(%dx%d) = %v", test.width, test.height, err)
			}
		})
	}
	if err := Validate([]byte("not a png")); err == nil {
		t.Fatal("accepted non-PNG data")
	}
	if err := Validate(make([]byte, maxBytes+1)); err == nil {
		t.Fatal("accepted oversized data")
	}
}

func TestReadWriteAndNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"", ".hidden", "nested/name", "name.png ", "-dash"} {
		if ValidName(name) {
			t.Fatalf("accepted name %q", name)
		}
	}
	if _, err := Read(root, "microsoft-word"); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing asset: %v", err)
	}
	exists, err := Exists(root, "microsoft-word")
	if err != nil || exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	data := encode(t, Size, Size)
	if err := Write(root, "microsoft-word", data); err != nil {
		t.Fatal(err)
	}
	read, err := Read(root, "microsoft-word")
	if err != nil || !bytes.Equal(read, data) {
		t.Fatalf("Read = %d bytes, %v", len(read), err)
	}
	if exists, err := Exists(root, "microsoft-word"); err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	if Path(root, "microsoft-word") != filepath.Join(root, "icons", "microsoft-word.png") || Relative("x") != "icons/x.png" {
		t.Fatal("asset paths diverged from the icons namespace")
	}
	if err := Write(root, "bad", encode(t, 10, 10)); err == nil {
		t.Fatal("wrote an invalid asset")
	}
	if err := os.WriteFile(Path(root, "corrupt"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root, "corrupt"); err == nil || errors.Is(err, ErrMissing) {
		t.Fatalf("corrupt asset: %v", err)
	}
}
