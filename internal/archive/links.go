package archive

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Readlink reads a confined relative link.
func Readlink(root *os.Root, name string) (string, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("not a symlink: %s", name)
	}
	target, err := root.Readlink(name)
	if err != nil {
		return "", err
	}
	// A POSIX target may hold any byte but NUL, including a backslash; only
	// Windows spells its own separator that way.
	if runtime.GOOS == "windows" {
		target = filepath.ToSlash(target)
	}
	resolved := path.Join(path.Dir(filepath.ToSlash(name)), target)
	if target == "" || strings.ContainsRune(target, 0) || path.IsAbs(target) || filepath.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("escaping symlink %s", name)
	}
	return target, nil
}
