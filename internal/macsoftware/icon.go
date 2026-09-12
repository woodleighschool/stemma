package macsoftware

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

// Optional icons are extracted from declared PNG files or PNG-backed ICNS entries.
// Asset catalogs and legacy ICNS encodings remain unavailable.
func applicationIcon(ctx context.Context, app, workspace string) (plugin.Artifact, error) {
	root, err := os.OpenRoot(app)
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = root.Close() }()
	data, err := readAppFile(ctx, root, "Contents/Info.plist", 4<<20)
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
	data, err = readAppFile(ctx, root, "Contents/Resources/"+info.Icon, 32<<20)
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
	return describeArtifact(ctx, output, "png")
}

func readAppFile(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.Open(name)
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
