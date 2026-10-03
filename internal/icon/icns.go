package icon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"

	"github.com/jackmordaunt/icns/v4"
)

// FromICNS decodes native icon elements and normalizes the largest artwork.
func FromICNS(data []byte) ([]byte, error) {
	if len(data) < 8 || int64(binary.BigEndian.Uint32(data[4:8])) != int64(len(data)) {
		return nil, errors.New("invalid ICNS length")
	}
	decoder, err := icns.NewDecoder(bytes.NewReader(data))
	if errors.Is(err, icns.ErrNoIcons) {
		return nil, fmt.Errorf("ICNS: %w", ErrUnsupportedArtwork)
	}
	if err != nil {
		return nil, err
	}
	var candidates []candidate
	for _, entry := range decoder.Icons() {
		candidates = append(candidates, candidate{data: entry.Payload(), decode: func() (image.Image, error) {
			switch entry.ImageFormat {
			case icns.ImageFormatRGB, icns.ImageFormatARGB, icns.ImageFormatBitmap, icns.ImageFormatIndexed:
				return entry.Decode()
			case icns.ImageFormatPNG, icns.ImageFormatJPEG2000:
				return decodeRaster(entry.Payload())
			default:
				return nil, ErrUnsupportedArtwork
			}
		}})
	}
	return largest(candidates)
}
