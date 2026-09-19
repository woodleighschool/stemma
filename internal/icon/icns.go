package icon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/png"
)

// FromICNS returns the largest PNG-encoded entry of an ICNS file that fits the
// asset bounds. Legacy run-length encodings and JPEG 2000 entries are not
// artwork here, so a file holding only those reports ErrNoArtwork.
func FromICNS(data []byte) ([]byte, error) {
	if len(data) < 8 || string(data[:4]) != "icns" || int64(binary.BigEndian.Uint32(data[4:8])) != int64(len(data)) {
		return nil, errors.New("not an ICNS file")
	}
	var entries [][]byte
	for offset := 8; offset+8 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if length < 8 || length > len(data)-offset {
			return nil, errors.New("ICNS entry exceeds the file")
		}
		entries = append(entries, data[offset+8:offset+length])
		offset += length
	}
	return largest(entries)
}

// largest picks the biggest candidate that is a valid asset on its own.
func largest(candidates [][]byte) ([]byte, error) {
	var best []byte
	edge := 0
	for _, candidate := range candidates {
		if Validate(candidate) != nil {
			continue
		}
		config, err := png.DecodeConfig(bytes.NewReader(candidate))
		if err == nil && config.Width > edge {
			best, edge = candidate, config.Width
		}
	}
	if best == nil {
		return nil, ErrNoArtwork
	}
	return best, nil
}
