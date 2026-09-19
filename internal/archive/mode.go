package archive

import (
	"errors"
	"io/fs"
)

// CheckMode rejects the permission bits that trees and packages do not carry.
func CheckMode(info fs.FileInfo) error {
	if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return errors.New("special permission bits are unsupported")
	}
	return nil
}
