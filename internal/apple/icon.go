package apple

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

const maxIconFile = 32 << 20

// IconPath resolves CFBundleIconFile to a confined resource name, adding the
// conventional extension when absent. An undeclared icon has no path.
func IconPath(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if name == "." || !fs.ValidPath(name) || path.Base(name) != name || strings.ContainsAny(name, "\\:\x00") {
		return "", fmt.Errorf("unsafe CFBundleIconFile %q", name)
	}
	if path.Ext(name) == "" {
		name += ".icns"
	}
	return "Contents/Resources/" + name, nil
}

// AppIconFile reads only the declared icon. Missing artwork returns nil;
// symlinks and unsafe resource names are rejected.
func AppIconFile(appPath, name string) ([]byte, error) {
	resource, err := IconPath(name)
	if err != nil || resource == "" {
		return nil, err
	}
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, err := readRegular(rootFS(root), resource, maxIconFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}
