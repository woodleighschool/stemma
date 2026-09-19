// Package testarchive builds archive fixtures from synthetic files.
package testarchive

import (
	"archive/zip"
	"os"
	"testing"
)

// Zip creates a ZIP containing sourceDir's children, as a release asset holds them.
func Zip(t testing.TB, filename, sourceDir string) {
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
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
