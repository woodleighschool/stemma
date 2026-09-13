package archive

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativeSelectedMetadata(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "payload")
	if err := os.WriteFile(file, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(dir, "stemma.test", []byte("unsupported"), 0); err != nil {
		t.Fatal(err)
	}
	if err := Pack(t.Context(), dir, io.Discard); err == nil {
		t.Fatal("whole-tree import accepted unsupported root metadata")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := PackSelected(t.Context(), root, []string{"payload"}, io.Discard); err != nil {
		t.Fatalf("unselected root metadata blocked payload: %v", err)
	}
	if err := unix.Setxattr(file, "stemma.test", []byte("unsupported"), 0); err != nil {
		t.Fatal(err)
	}
	if err := PackSelected(t.Context(), root, []string{"payload"}, io.Discard); err == nil {
		t.Fatal("selected payload accepted unsupported metadata")
	}
}

func TestNativePayloadMetadata(t *testing.T) {
	for _, tc := range []struct {
		name  string
		xattr string
		flags int
		want  string
	}{
		{name: "resource fork", xattr: "com.apple.ResourceFork", want: "extended attribute"},
		{name: "custom attribute", xattr: "stemma.required", want: "extended attribute"},
		{name: "additional flags", flags: unix.UF_TRACKED | unix.UF_HIDDEN, want: "BSD file flags"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "payload")
			if err := os.WriteFile(filename, []byte("payload"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(filename, "com.apple.quarantine", []byte("0081;00000000;Fixture;"), 0); err != nil {
				t.Fatal(err)
			}
			if tc.xattr != "" {
				if err := unix.Setxattr(filename, tc.xattr, []byte("required metadata"), 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := unix.Chflags(filename, tc.flags); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(filename)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckMetadata(f, info); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("payload metadata was not rejected: %v", err)
			}
		})
	}
}
