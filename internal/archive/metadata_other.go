//go:build !darwin && !linux && !windows

package archive

import (
	"errors"
	"os"
)

func checkMetadata(*os.File, os.FileInfo) error {
	return errors.New("filesystem metadata inspection is unsupported on this host")
}

func checkSymlinkMetadata(*os.Root, string, os.FileInfo) error {
	return errors.New("symlink metadata inspection is unsupported on this host")
}
