// Package icon exports optional application icons as retained, portable PNGs.
package icon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/fileio"
)

const maxImageBytes = 32 << 20

// Options controls explicit rendering. Existing PNGs are reused unless Refresh is set.
type Options struct {
	Size    int
	Refresh bool
}

// Result records the retained image's content and rendering provenance.
type Result struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Source    string `json:"source"`
	Renderer  string `json:"renderer"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	OSVersion string `json:"os_version,omitempty"`
	OSBuild   string `json:"os_build,omitempty"`
	Reused    bool   `json:"reused"`
}

// Export saves a PNG and its provenance beside it. System rendering requires macOS;
// supplied images and retained PNGs work on every platform.
func Export(ctx context.Context, source, output string, options Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if output == "" || !strings.EqualFold(filepath.Ext(output), ".png") {
		return Result{}, errors.New("icon output must be a .png path")
	}
	output, err := filepath.Abs(output)
	if err != nil {
		return Result{}, err
	}
	if !options.Refresh {
		data, err := readFile(ctx, output)
		if err == nil {
			result, err := describe(data)
			if err != nil {
				return Result{}, fmt.Errorf("retained icon: %w", err)
			}
			if metadata, err := os.ReadFile(output + ".json"); err == nil {
				var previous Result
				if json.Unmarshal(metadata, &previous) == nil && previous.SHA256 == result.SHA256 {
					result = previous
				}
			}
			result.Path, result.Reused = output, true
			return result, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
	}
	if options.Size == 0 {
		options.Size = 256
	}
	if options.Size < 1 || options.Size > 4096 {
		return Result{}, errors.New("icon size must be between 1 and 4096 pixels")
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return Result{}, err
	}
	var data []byte
	result := Result{Source: source, Renderer: "quicklook"}
	switch {
	case strings.EqualFold(filepath.Ext(source), ".png"):
		data, err = readFile(ctx, source)
		result.Renderer = "supplied"
	default:
		data, result.OSVersion, result.OSBuild, err = render(ctx, source, options.Size)
	}
	if err != nil {
		return Result{}, err
	}
	image, err := describe(data)
	if err != nil {
		return Result{}, err
	}
	result.Path, result.SHA256 = output, image.SHA256
	result.Width, result.Height = image.Width, image.Height
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	metadata, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := fileio.Write(output, data, 0o644); err != nil {
		return Result{}, err
	}
	if err := fileio.Write(output+".json", append(metadata, '\n'), 0o644); err != nil {
		return result, err
	}
	return result, nil
}

func describe(data []byte) (Result, error) {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("icon must be a valid PNG: %w", err)
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return Result{}, errors.New("icon dimensions exceed 4096 pixels")
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return Result{}, fmt.Errorf("icon PNG: %w", err)
	}
	digest := sha256.Sum256(data)
	return Result{SHA256: hex.EncodeToString(digest[:]), Renderer: "supplied", Width: config.Width, Height: config.Height}, nil
}

func readFile(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return read(ctx, f)
}

func read(ctx context.Context, r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: r}, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, errors.New("icon input exceeds 32 MiB")
	}
	return data, nil
}
