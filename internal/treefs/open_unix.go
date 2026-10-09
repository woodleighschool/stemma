//go:build unix

package treefs

import (
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (l local) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return l.root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
