package apple

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"

	"github.com/woodleighschool/stemma/internal/archive"
	"howett.net/plist"
)

const maxIconFile = 32 << 20

// IconResources names the files of a bundle that the system icon renderer
// reads: its metadata, the main executable that marks it as launchable, and
// the icon files and asset catalog at the top of Resources.
func IconResources(executable string) archive.Leaves {
	return archive.Leaves{
		"Contents/Info.plist",
		"Contents/PkgInfo",
		"Contents/MacOS/" + archive.Literal(executable),
		"Contents/Resources/*.icns",
		"Contents/Resources/Assets.car",
	}
}

// AppIconFile reads the icon file a bundle declares through CFBundleIconFile,
// with the conventional .icns extension when the name has none. A bundle
// declaring none, or declaring a file it does not carry, returns nil.
func AppIconFile(appPath string) ([]byte, error) {
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	bundle := rootFS(root)
	data, err := readRegular(bundle, "Contents/Info.plist", maxMetadata)
	if err != nil {
		return nil, err
	}
	var info struct {
		Icon string `plist:"CFBundleIconFile"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("app Info.plist: %w", err)
	}
	if info.Icon == "" {
		return nil, nil
	}
	if !fs.ValidPath(info.Icon) || path.Base(info.Icon) != info.Icon {
		return nil, fmt.Errorf("unsafe CFBundleIconFile %q", info.Icon)
	}
	name := info.Icon
	if path.Ext(name) == "" {
		name += ".icns"
	}
	data, err = readRegular(bundle, "Contents/Resources/"+name, maxIconFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
