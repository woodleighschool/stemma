// Package testproject writes project fixtures for tests.
package testproject

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Write stores the manifest's first document at filename and any further
// documents beside it as one imported family file.
func Write(t testing.TB, filename, manifest string) {
	t.Helper()
	project, resources, found := strings.Cut(manifest, "\n---\n")
	write(t, filename, project)
	if found {
		write(t, filepath.Join(filepath.Dir(filename), "resources.software.yaml"), resources)
	}
}

func write(t testing.TB, filename, data string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
