package intunewin

import "syscall"

func publishDirectory(stage, destination string) error {
	from, err := syscall.UTF16PtrFromString(stage)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// MoveFile, unlike MoveFileEx with replace, refuses an existing destination.
	return syscall.MoveFile(from, to)
}
