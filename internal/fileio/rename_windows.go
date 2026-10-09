package fileio

import "golang.org/x/sys/windows"

// RenameExclusive publishes a private file or tree without replacing any
// existing destination. Both paths must be on the same filesystem.
func RenameExclusive(oldPath, newPath string) error {
	from, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	return windows.MoveFile(from, to)
}
