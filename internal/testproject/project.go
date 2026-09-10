// Package testproject writes multi-document test fixtures as individual files.
package testproject

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// Write stores the first document at filename and subsequent software documents
// beside it. Production configuration still requires one document per file.
func Write(filename string, data []byte) error {
	documents := bytes.Split(data, []byte("\n---\n"))
	for i, document := range documents {
		target := filename
		if i > 0 {
			target = filepath.Join(filepath.Dir(filename), fmt.Sprintf("%d.software.yaml", i))
		}
		if err := os.WriteFile(target, document, 0o600); err != nil {
			return err
		}
	}
	return nil
}
