// Package testdiskimage builds disk image fixtures from synthetic files.
package testdiskimage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
)

// Write creates a zlib-compressed HFSX DMG containing sourceDir's children.
func Write(t testing.TB, filename, sourceDir string) {
	t.Helper()
	volume, err := os.Create(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = volume.Close() }()
	if err := hfsplus.CreateImageFromDir(volume, 0, "Fixture", sourceDir, nil); err != nil {
		t.Fatal(err)
	}
	info, err := volume.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.WrapRawImageDMGFrom(filename, volume, info.Size(), "Apple_HFSX", nil); err != nil {
		t.Fatal(err)
	}
}
