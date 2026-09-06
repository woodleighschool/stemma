package archive

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeWindowsMetadata(t *testing.T) {
	for _, kind := range []string{"file stream", "directory stream", "hard link", "junction"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			filename := filepath.Join(root, "file")
			if err := os.WriteFile(filename, []byte("payload"), 0o644); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file stream":
				if err := os.WriteFile(filename+":extra", []byte("metadata"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory stream":
				if err := os.WriteFile(root+":extra", []byte("metadata"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "hard link":
				if err := os.Link(filename, filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			case "junction":
				command := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", filepath.Join(root, "junction"), t.TempDir())
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("create junction: %v\n%s", err, output)
				}
			}
			if err := Pack(t.Context(), root, io.Discard); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

func TestNativeSymlinkMetadata(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "file")
	if err := os.WriteFile(filename, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename+":extra", []byte("metadata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	scoped, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = scoped.Close() }()
	if err := PackSelected(t.Context(), scoped, []string{"link", "dangling"}, io.Discard); err != nil {
		t.Fatalf("symlink inherited target metadata: %v", err)
	}
}
