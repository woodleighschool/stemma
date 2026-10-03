package icon

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // Register native raster artwork formats.
	_ "image/jpeg" // Register native raster artwork formats.
	"image/png"

	_ "github.com/mrjoshuak/go-jpeg2000" // Register the JPEG 2000 codec used by ICNS.
	_ "golang.org/x/image/bmp"           // Register native raster artwork formats.
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff" // Register native raster artwork formats.
	_ "golang.org/x/image/webp" // Register native raster artwork formats.
)

// ErrUnsupportedArtwork reports artwork present in an unsupported encoding.
var ErrUnsupportedArtwork = errors.New("artwork uses an unsupported encoding")

const maxSourceEdge = 4096

// FromImage reads a native icon or raster image into the catalog PNG contract.
func FromImage(data []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(data, []byte("icns")):
		return FromICNS(data)
	case bytes.HasPrefix(data, []byte{0, 0, 1, 0}):
		return FromICO(data)
	default:
		return largest([]candidate{{data: data, decode: func() (image.Image, error) { return decodeRaster(data) }}})
	}
}

type candidate struct {
	data   []byte
	decode func() (image.Image, error)
}

func decodeRaster(data []byte) (image.Image, error) {
	// Containers are decoded explicitly; their frames must be raster images.
	if bytes.HasPrefix(data, []byte("icns")) || bytes.HasPrefix(data, []byte{0, 0, 1, 0}) {
		return nil, errors.New("nested icon container")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if errors.Is(err, image.ErrFormat) {
		return nil, ErrUnsupportedArtwork
	}
	if err != nil {
		return nil, err
	}
	if err := checkDimensions(config.Width, config.Height); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

func checkDimensions(width, height int) error {
	if width < 1 || height < 1 || width > maxSourceEdge || height > maxSourceEdge {
		return fmt.Errorf("artwork dimensions %dx%d exceed bounds 1..%d", width, height, maxSourceEdge)
	}
	return nil
}

// largest retains a usable frame when another cannot be read, but reports
// decoding failures when none can. Asset bounds apply after normalization.
func largest(candidates []candidate) ([]byte, error) {
	var best image.Image
	var valid []byte
	var failures []error
	area, validArea := 0, 0
	for _, candidate := range candidates {
		img, err := candidate.decode()
		if err == nil {
			err = checkDimensions(img.Bounds().Dx(), img.Bounds().Dy())
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		pixels := img.Bounds().Dx() * img.Bounds().Dy()
		if pixels > validArea && Validate(candidate.data) == nil {
			valid, validArea = candidate.data, pixels
		}
		if pixels > area {
			best, area = img, pixels
		}
	}
	if best == nil {
		if len(failures) > 0 {
			return nil, fmt.Errorf("decode artwork: %w", errors.Join(failures...))
		}
		return nil, ErrNoArtwork
	}
	if valid != nil {
		return valid, nil
	}
	bounds := best.Bounds()
	longest := max(bounds.Dx(), bounds.Dy())
	edge := max(minEdge, min(maxEdge, longest))
	for {
		img := best
		if bounds.Dx() != edge || bounds.Dy() != edge {
			canvas := image.NewNRGBA(image.Rect(0, 0, edge, edge))
			width, height := max(1, bounds.Dx()*edge/longest), max(1, bounds.Dy()*edge/longest)
			at := image.Pt((edge-width)/2, (edge-height)/2)
			draw.CatmullRom.Scale(canvas, image.Rectangle{Min: at, Max: at.Add(image.Pt(width, height))}, best, bounds, draw.Src, nil)
			img = canvas
		}
		var output bytes.Buffer
		if err := png.Encode(&output, img); err != nil {
			return nil, err
		}
		if output.Len() > maxBytes && edge > minEdge {
			edge = max(minEdge, edge/2)
			continue
		}
		if err := Validate(output.Bytes()); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
}
