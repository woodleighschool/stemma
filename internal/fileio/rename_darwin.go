package fileio

import "golang.org/x/sys/unix"

// RenameExclusive publishes a private file or tree without replacing any
// existing destination. Both paths must be on the same filesystem.
func RenameExclusive(oldPath, newPath string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_EXCL)
}
