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
	root, _, err := hfsplus.EntryTreeFromDir(sourceDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for entries := []*hfsplus.Entry{root}; len(entries) > 0; entries = entries[1:] {
		entry := entries[0]
		if entry.Mode&os.ModeSymlink != 0 {
			entry.Data = []byte(filepath.ToSlash(string(entry.Data)))
		}
		entries = append(entries, entry.Children...)
	}
	volume, err := os.Create(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = volume.Close() }()
	if err := hfsplus.CreateImage(volume, 0, "Fixture", root, nil); err != nil {
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
