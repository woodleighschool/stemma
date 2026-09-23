//go:build unix

package archive

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPackIdentitySurvivesUmask(t *testing.T) {
	if os.Getenv("STEMMA_TEST_TREE_UMASK") != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPackIdentitySurvivesUmask$")
		command.Env = append(os.Environ(), "STEMMA_TEST_TREE_UMASK=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("umask subprocess: %v\n%s", err, output)
		}
		return
	}
	// Isolate the process-wide umask from other tests and their goroutines.
	previous := unix.Umask(0o022)
	defer unix.Umask(previous)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Versions/A"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Versions/A/library"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("A", filepath.Join(root, "Versions/Current")); err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	if err := Pack(t.Context(), root, &original); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "tree.tar")
	if err := os.WriteFile(input, original.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mask := range []int{0o077, 0o022} {
		unix.Umask(mask)
		leased := filepath.Join(t.TempDir(), "leased")
		if err := Extract(t.Context(), input, leased); err != nil {
			t.Fatal(err)
		}
		for name, mode := range map[string]os.FileMode{"Versions/A": 0o750, "Versions/A/library": 0o640} {
			if info, err := os.Stat(filepath.Join(leased, name)); err != nil || info.Mode().Perm() != mode {
				t.Fatalf("mode for %s changed under umask %03o: %v, %v", name, mask, info, err)
			}
		}
		var packed bytes.Buffer
		if err := Pack(t.Context(), leased, &packed); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(original.Bytes(), packed.Bytes()) {
			t.Fatalf("tree identity changed under umask %03o", mask)
		}
		if err := os.Remove(filepath.Join(leased, "Versions/Current")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("A/library", filepath.Join(leased, "Versions/Current")); err != nil {
			t.Fatal(err)
		}
		packed.Reset()
		if err := Pack(t.Context(), leased, &packed); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(original.Bytes(), packed.Bytes()) {
			t.Fatal("changed link target did not change tree identity")
		}
	}
}
