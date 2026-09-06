package archive

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPackSelectedChecksOnlyIncludedMetadata(t *testing.T) {
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
