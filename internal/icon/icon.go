// Package icon owns the catalog icon namespace. A resource declares an icon by
// name, the asset lives at icons/<name>.png and destinations publish those
// exact bytes. Authoring extracts the artwork software carries into a Subject
// on any host and a Presentation styles it into the asset.
package icon

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"

	"github.com/woodleighschool/stemma/internal/fileio"
)

const (
	// Directory holds every icon asset, relative to the project root.
	Directory = "icons"
	// Size is the canonical rendered edge in pixels: crisp at 2x for the
	// 256-point presentations destinations use today without upscaling.
	Size = 512

	minEdge  = 128
	maxEdge  = 1024
	maxBytes = 1 << 20
)

// ErrMissing reports a declared asset without a file; stemma icon renders it.
var ErrMissing = errors.New("icon asset does not exist")

// ErrUnsupportedHost reports a presentation this host cannot draw.
var ErrUnsupportedHost = errors.New("the glassy presentation requires macOS")

// ErrNoArtwork reports software that carries nothing an icon can be made from.
var ErrNoArtwork = errors.New("no icon artwork")

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidName accepts a bare asset name without directories or extension.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

// Path locates the asset for a declared name inside a project root.
func Path(root, name string) string {
	return filepath.Join(root, Directory, name+".png")
}

// Relative names the asset the way documents and messages refer to it.
func Relative(name string) string {
	return path.Join(Directory, name+".png")
}

// Validate accepts a square PNG between 128 and 1024 pixels of at most 1 MiB.
func Validate(data []byte) error {
	if len(data) > maxBytes {
		return fmt.Errorf("icon exceeds %d bytes", maxBytes)
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("icon must be a PNG: %w", err)
	}
	if config.Width != config.Height || config.Width < minEdge || config.Width > maxEdge {
		return fmt.Errorf("icon must be square between %d and %d pixels, not %dx%d", minEdge, maxEdge, config.Width, config.Height)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("icon PNG: %w", err)
	}
	return nil
}

// Read loads and validates a declared asset. A missing file wraps ErrMissing.
func Read(root, name string) ([]byte, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("icon %q must name an asset under %s/", name, Directory)
	}
	file, err := os.Open(Path(root, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", Relative(name), ErrMissing)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Relative(name), err)
	}
	if err := Validate(data); err != nil {
		return nil, fmt.Errorf("%s: %w", Relative(name), err)
	}
	return data, nil
}

// Exists reports whether a declared asset has a file, without validating it.
func Exists(root, name string) (bool, error) {
	_, err := os.Stat(Path(root, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Write validates data and replaces the asset for name.
func Write(root, name string, data []byte) error {
	if !ValidName(name) {
		return fmt.Errorf("icon %q must name an asset under %s/", name, Directory)
	}
	if err := Validate(data); err != nil {
		return err
	}
	target := Path(root, name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return fileio.Write(target, data, 0o644)
}
