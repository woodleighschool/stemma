// Package testdiskimage builds portable disk image fixtures from synthetic files.
package testdiskimage

import (
	"os"
	"path/filepath"

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
)

// Write creates a zlib-compressed HFSX DMG containing sourceDir's children.
// It is intended for synthetic fixture trees, not vendor payload packaging.
func Write(filename, sourceDir string) error {
	f, err := os.CreateTemp(filepath.Dir(filename), ".test-volume-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if err := hfsplus.CreateImageFromDir(f, 0, "Fixture", sourceDir, nil); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return disk.WrapRawImageDMGFrom(filename, f, info.Size(), "Apple_HFSX", nil)
}
