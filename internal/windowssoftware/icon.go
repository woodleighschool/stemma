package windowssoftware

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/msi"
	"github.com/woodleighschool/stemma/plugin"
)

var compoundSignature = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

// Icon extracts the application artwork a prepared installer carries: the
// icon an MSI registers for Programs and Features, or the first icon group of
// a setup executable. Installers without one report icon.ErrNoArtwork.
func Icon(ctx context.Context, installer plugin.Artifact) (icon.Subject, error) {
	if err := ctx.Err(); err != nil {
		return icon.Subject{}, err
	}
	setup := installer.Path
	if installer.Tree {
		setup = filepath.Join(installer.Path, filepath.FromSlash(installer.EntryPoint))
	}
	file, err := os.Open(setup)
	if err != nil {
		return icon.Subject{}, err
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, len(compoundSignature))
	if _, err := io.ReadFull(file, header); err != nil {
		return icon.Subject{}, icon.ErrNoArtwork
	}
	var artwork []byte
	switch {
	case bytes.Equal(header, compoundSignature):
		data, ok, err := msi.ProductIcon(setup)
		if err != nil {
			return icon.Subject{}, err
		}
		if !ok {
			return icon.Subject{}, icon.ErrNoArtwork
		}
		// The registered icon is an ICO file or an executable carrying one.
		if bytes.HasPrefix(data, []byte("MZ")) {
			artwork, err = icon.FromPE(bytes.NewReader(data))
		} else {
			artwork, err = icon.FromICO(data)
		}
		if err != nil {
			return icon.Subject{}, fmt.Errorf("MSI product icon: %w", err)
		}
	case bytes.HasPrefix(header, []byte("MZ")):
		if artwork, err = icon.FromPE(file); err != nil {
			return icon.Subject{}, fmt.Errorf("setup executable: %w", err)
		}
	default:
		return icon.Subject{}, icon.ErrNoArtwork
	}
	return icon.Subject{Artwork: artwork}, nil
}
