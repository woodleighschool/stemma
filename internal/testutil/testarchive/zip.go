// Package testarchive builds archive fixtures from synthetic files.
package testarchive

import (
	"archive/zip"
	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"maps"
	"os"
	"slices"
	"testing"
)

// Zip creates a ZIP containing sourceDir's children, as a release asset holds them.
func Zip(t testing.TB, filename, sourceDir string) {
	t.Helper()
	ZipWithAttributes(t, filename, sourceDir, nil)
}

// ZipWithAttributes adds synthetic AppleDouble members to an ordinary ZIP.
func ZipWithAttributes(t testing.TB, filename, sourceDir string, attributes map[string]map[string][]byte) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	writer := zip.NewWriter(file)
	if err := writer.AddFS(os.DirFS(sourceDir)); err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(attributes)) {
		data, err := appledouble.FromXattrs(attributes[name]).Encode()
		if err != nil {
			t.Fatal(err)
		}
		entry, err := writer.Create("__MACOSX/" + appledouble.SidecarName(name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
