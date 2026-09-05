package archive

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func checkSymlinkMetadata(root *os.Root, name string, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
		return errors.New("hard links are unsupported")
	}
	count, err := unix.Llistxattr(filepath.Join(root.Name(), filepath.FromSlash(name)), nil)
	if err != nil {
		return err
	}
	if count != 0 {
		return errors.New("symlink extended attributes are unsupported")
	}
	return nil
}

// Linux POSIX ACLs are rejected with their system.posix_acl_* extended attributes.
func checkACL(*os.File, os.FileInfo) error { return nil }
