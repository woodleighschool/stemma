package archive

import (
	"fmt"
	"io/fs"
)

// MatchPath resolves an archive path or glob to exactly one entry.
func MatchPath(files fs.FS, selection string) (string, error) {
	if _, err := safeName(selection); err != nil {
		return "", err
	}
	matches, err := fs.Glob(files, selection)
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("archive selection %q matched %d entries; exactly one is required", selection, len(matches))
	}
	return matches[0], nil
}
