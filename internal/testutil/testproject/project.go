// Package testproject writes project fixtures for tests.
package testproject

import (
	"io/fs"
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

// ReadLock returns a project's lock documents in path order, each after its
// path, as one reads a single file. A project that locks nothing has none.
func ReadLock(root string) ([]byte, error) {
	var data []byte
	project := os.DirFS(root)
	err := fs.WalkDir(project, "stemma.lock", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		document, err := fs.ReadFile(project, name)
		data = append(append(data, name+"\n"...), document...)
		return err
	})
	return data, err
}
