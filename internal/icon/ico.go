package icon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"

	"github.com/jackmordaunt/icns/v4/ico"
)

// FromICO decodes PNG and bitmap frames, including their transparency masks.
func FromICO(data []byte) ([]byte, error) {
	decoder, err := ico.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var candidates []candidate
	for _, entry := range decoder.Icons() {
		candidates = append(candidates, candidate{data: entry.Payload(), decode: func() (image.Image, error) {
			if entry.Format == ico.FormatPNG {
				return decodeRaster(entry.Payload())
			}
			data := entry.Payload()
			if len(data) < 12 {
				return nil, errors.New("truncated icon bitmap")
			}
			// Negative bitmap dimensions exceed the bounds when read unsigned.
			width, height := int(binary.LittleEndian.Uint32(data[4:])), int(binary.LittleEndian.Uint32(data[8:]))
			if err := checkDimensions(width, height/2); err != nil {
				return nil, err
			}
			return entry.Decode()
		}})
	}
	return largest(candidates)
}
