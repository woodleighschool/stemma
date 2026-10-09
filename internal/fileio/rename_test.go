package fileio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenameExclusive(t *testing.T) {
	for _, tree := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			dir := t.TempDir()
			source, target := filepath.Join(dir, "source"), filepath.Join(dir, "target")
			create := func(name string) {
				t.Helper()
				var err error
				if tree {
					err = os.Mkdir(name, 0o700)
				} else {
					err = os.WriteFile(name, []byte("payload"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			create(source)
			if exists {
				create(target)
			}
			err := RenameExclusive(source, target)
			if (err != nil) != exists {
				t.Fatalf("tree=%t exists=%t: %v", tree, exists, err)
			}
			_, err = os.Stat(source)
			if os.IsNotExist(err) == exists {
				t.Fatalf("tree=%t exists=%t: source lost or retained: %v", tree, exists, err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatal(err)
			}
		}
	}
}
