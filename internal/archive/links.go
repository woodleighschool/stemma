package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Readlink reads a confined relative link and checks its filesystem metadata.
func Readlink(root *os.Root, name string) (string, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("not a symlink: %s", name)
	}
	if err := checkSymlinkMetadata(root, name, info); err != nil {
		return "", err
	}
	target, err := root.Readlink(name)
	if err != nil {
		return "", err
	}
	target = filepath.ToSlash(target)
	resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
	if filepath.IsAbs(target) || !filepath.IsLocal(resolved) || strings.Contains(target, "\\") {
		return "", fmt.Errorf("escaping symlink %s", name)
	}
	return target, nil
}
