package macsoftware

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

// Optional icons are extracted from declared PNG files or PNG-backed ICNS entries.
// Asset catalogs and legacy ICNS encodings remain unavailable.
func portableIcon(ctx context.Context, app, workspace string) (plugin.Artifact, error) {
	root, err := os.OpenRoot(app)
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = root.Close() }()
	return portableIconFS(ctx, root.FS(), ".", workspace)
}

func portableIconFS(ctx context.Context, fsys fs.FS, app, workspace string) (plugin.Artifact, error) {
	data, err := readAppFile(ctx, fsys, path.Join(app, "Contents/Info.plist"), 4<<20)
	if err != nil {
		return plugin.Artifact{}, err
	}
	var info struct {
		Icon string `plist:"CFBundleIconFile"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil {
		return plugin.Artifact{}, err
	}
	if info.Icon == "" {
		return plugin.Artifact{}, nil
	}
	if !relativePath(info.Icon) || path.Base(info.Icon) != info.Icon {
		return plugin.Artifact{}, errors.New("application icon must name a resource file")
	}
	if path.Ext(info.Icon) == "" {
		info.Icon += ".icns"
	}
	data, err = readAppFile(ctx, fsys, path.Join(app, "Contents/Resources", info.Icon), 32<<20)
	if errors.Is(err, os.ErrNotExist) {
		return plugin.Artifact{}, nil
	}
	if err != nil {
		return plugin.Artifact{}, err
	}
	if strings.EqualFold(path.Ext(info.Icon), ".icns") {
		data = icnsPNG(data)
	}
	if !validPNG(data) {
		return plugin.Artifact{}, nil
	}
	output := filepath.Join(workspace, "icon.png")
	if err := fileio.Write(output, data, 0o644); err != nil {
		return plugin.Artifact{}, err
	}
	artifact, err := describeArtifact(ctx, output, "png")
	if artifact.Path != "" {
		artifact.Evidence = map[string]json.RawMessage{"macos.icon": json.RawMessage(`{"renderer":"portable/1"}`)}
	}
	return artifact, err
}

func readAppFile(ctx context.Context, fsys fs.FS, name string, limit int64) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: file}, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("application icon metadata exceeds size limit")
	}
	return data, nil
}

func icnsPNG(data []byte) []byte {
	if len(data) < 8 || string(data[:4]) != "icns" || int64(binary.BigEndian.Uint32(data[4:8])) != int64(len(data)) {
		return nil
	}
	var best []byte
	area := 0
	for offset := 8; offset+8 <= len(data); {
		length := int64(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if length < 8 || length > int64(len(data)-offset) {
			return nil
		}
		candidate := data[offset+8 : offset+int(length)]
		if config, err := png.DecodeConfig(bytes.NewReader(candidate)); err == nil && config.Width <= 4096 && config.Height <= 4096 && config.Width*config.Height > area {
			best, area = candidate, config.Width*config.Height
		}
		offset += int(length)
	}
	return best
}

func validPNG(data []byte) bool {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return false
	}
	_, err = png.Decode(bytes.NewReader(data))
	return err == nil
}

// IconVariant identifies the renderer without coupling installers to the host OS.
func IconVariant() string {
	if runtime.GOOS == "darwin" {
		return "macos-native/1"
	}
	return "portable/1"
}

func addIcon(ctx context.Context, outputs map[string]plugin.Artifact, app, workspace string) {
	artwork, err := applicationIcon(ctx, app, workspace)
	if err != nil {
		plugin.Logger(ctx).DebugContext(ctx, "Application icon unavailable", "error", err)
		return
	}
	if artwork.Path != "" {
		outputs["icon"] = artwork
	}
}

func applicationIcon(ctx context.Context, app, workspace string) (plugin.Artifact, error) {
	if runtime.GOOS == "darwin" {
		data, version, build, err := nativeIcon(ctx, app, 256)
		if err == nil && validPNG(data) {
			output := filepath.Join(workspace, "icon.png")
			if err := fileio.Write(output, data, 0o644); err != nil {
				return plugin.Artifact{}, err
			}
			artifact, err := describeArtifact(ctx, output, "png")
			evidence, _ := json.Marshal(map[string]string{"renderer": "macos-native/1", "os_version": version, "os_build": build})
			artifact.Evidence = map[string]json.RawMessage{"macos.icon": evidence}
			return artifact, err
		}
	}
	return portableIcon(ctx, app, workspace)
}
