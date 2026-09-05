//go:build !windows

package intunewin

import (
	"os"
	"syscall"
)

func publishDirectory(stage, destination string) error {
	// Reserve the name so Rename cannot replace an existing user directory.
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	if err := syscall.Rename(stage, destination); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}
