// Package testdiskimage builds disk image fixtures from synthetic files.
package testdiskimage

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
)

// Write creates a zlib-compressed HFSX DMG containing sourceDir's children.
func Write(t testing.TB, filename, sourceDir string) {
	t.Helper()
	WriteXattrs(t, filename, sourceDir, nil)
}

// WriteXattrs is Write with extended attributes for the files at slash-separated
// paths relative to sourceDir.
func WriteXattrs(t testing.TB, filename, sourceDir string, xattrs map[string]map[string][]byte) {
	t.Helper()
	root, _, err := hfsplus.EntryTreeFromDir(sourceDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	type located struct {
		entry *hfsplus.Entry
		path  string
	}
	for entries := []located{{root, "."}}; len(entries) > 0; entries = entries[1:] {
		entry := entries[0].entry
		if entry.Mode&os.ModeSymlink != 0 {
			entry.Data = []byte(filepath.ToSlash(string(entry.Data)))
		}
		entry.Xattrs = xattrs[entries[0].path]
		for _, child := range entry.Children {
			entries = append(entries, located{child, path.Join(entries[0].path, child.Name)})
		}
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
