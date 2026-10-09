//go:build unix

package treefs

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

type changingFS struct {
	fs.ReadLinkFS

	change func()
}

func (s changingFS) Open(name string) (fs.File, error) {
	s.change()
	return s.ReadLinkFS.Open(name)
}

func TestReplacementBetweenStatAndOpen(t *testing.T) {
	for _, replacement := range []string{"symlink", "fifo", "regular"} {
		t.Run(replacement, func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, "file")
			if err := os.WriteFile(name, []byte("payload"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			fsys := changingFS{ReadLinkFS: Local(root), change: func() {
				if err := os.Rename(name, name+".old"); err != nil {
					t.Fatal(err)
				}
				var err error
				switch replacement {
				case "symlink":
					err = os.Symlink("file.old", name)
				case "fifo":
					err = unix.Mkfifo(name, 0o600)
				case "regular":
					err = os.WriteFile(name, []byte("payload"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			f, err := OpenFile(fsys, "file")
			if err == nil {
				_ = f.Close()
				t.Fatal("accepted replacement between stat and open")
			}
		})
	}
}
