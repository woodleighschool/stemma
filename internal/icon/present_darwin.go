package icon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const hostPresentation = Glassy

// glassy draws the subject with the system icon renderer at size pixels, so
// the result carries the current macOS presentation. A real bundle is the
// richest source; artwork alone is staged in a surrogate bundle first.
func glassy(ctx context.Context, subject Subject, size int, workspace string) ([]byte, error) {
	source := subject.Path
	if source == "" {
		var err error
		if source, err = surrogate(subject, workspace); err != nil {
			return nil, err
		}
	}
	if !strings.EqualFold(filepath.Ext(source), ".app") {
		return nil, errors.New("glassy rendering requires a .app bundle")
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("glassy rendering requires a .app directory")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := quickLookPNG(ctx, source, size)
	if err != nil {
		return nil, fmt.Errorf("quick look icon rendering: %w", err)
	}
	return data, nil
}

// surrogate stages artwork as the icon of a minimal application bundle.
// The bundle needs an executable, because Launch Services badges
// software it cannot run as prohibited; a script is enough.
func surrogate(subject Subject, workspace string) (string, error) {
	icns, err := encodeICNS(subject.Artwork)
	if err != nil {
		return "", err
	}
	app := filepath.Join(workspace, "surrogate", "Application.app")
	for name, data := range map[string][]byte{
		"Contents/Info.plist": []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>application</string>
<key>CFBundleIconFile</key><string>icon</string>
<key>CFBundleIdentifier</key><string>local.stemma.icon.surrogate</string>
<key>CFBundleName</key><string>Application</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>
`),
		"Contents/MacOS/application":   []byte("#!/bin/sh\nexit 0\n"),
		"Contents/Resources/icon.icns": icns,
	} {
		target := filepath.Join(app, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(name, "Contents/MacOS/") {
			mode = 0o755
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return "", err
		}
	}
	return app, nil
}

// icnsTypes name the PNG-backed ICNS entries by edge; other edges have none.
var icnsTypes = map[int]string{128: "ic07", 256: "ic08", 512: "ic09", 1024: "ic10"}

func encodeICNS(artwork []byte) ([]byte, error) {
	config, err := png.DecodeConfig(bytes.NewReader(artwork))
	if err != nil {
		return nil, fmt.Errorf("icon artwork: %w", err)
	}
	kind, ok := icnsTypes[config.Width]
	if !ok || config.Height != config.Width {
		return nil, fmt.Errorf("artwork of %dx%d pixels has no ICNS representation", config.Width, config.Height)
	}
	length := len(artwork)
	if length > maxBytes {
		return nil, fmt.Errorf("artwork exceeds %d bytes", maxBytes)
	}
	var buf bytes.Buffer
	buf.WriteString("icns")
	_ = binary.Write(&buf, binary.BigEndian, uint32(16+length))
	buf.WriteString(kind)
	_ = binary.Write(&buf, binary.BigEndian, uint32(8+length))
	buf.Write(artwork)
	return buf.Bytes(), nil
}
