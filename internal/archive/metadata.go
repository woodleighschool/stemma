package archive

import (
	"errors"
	"os"
)

// CheckMetadata rejects filesystem metadata that the portable tree and package
// representations cannot preserve. The descriptor must identify the stated file.
func CheckMetadata(f *os.File, info os.FileInfo) error {
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("special permission bits are unsupported")
	}
	return checkMetadata(f, info)
}
