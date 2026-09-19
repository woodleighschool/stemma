package icon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// FromICO returns the largest frame of an ICO file that fits the asset bounds.
// PNG frames keep their bytes; bitmap frames are decoded with their mask and
// encoded as PNG.
func FromICO(data []byte) ([]byte, error) {
	frames, err := icoFrames(data)
	if err != nil {
		return nil, err
	}
	return largest(framesPNG(frames))
}

func icoFrames(data []byte) ([][]byte, error) {
	if len(data) < 6 || binary.LittleEndian.Uint16(data) != 0 || binary.LittleEndian.Uint16(data[2:]) != 1 {
		return nil, errors.New("not an ICO file")
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	if len(data) < 6+16*count {
		return nil, errors.New("ICO directory exceeds the file")
	}
	frames := make([][]byte, 0, count)
	for i := range count {
		entry := data[6+16*i:]
		size, offset := int64(binary.LittleEndian.Uint32(entry[8:])), int64(binary.LittleEndian.Uint32(entry[12:]))
		if offset+size > int64(len(data)) {
			return nil, errors.New("ICO frame exceeds the file")
		}
		frames = append(frames, data[offset:offset+size])
	}
	return frames, nil
}

// framesPNG converts icon frames to PNG, dropping those no decoder accepts.
func framesPNG(frames [][]byte) [][]byte {
	var result [][]byte
	for _, frame := range frames {
		if bytes.HasPrefix(frame, pngSignature) {
			result = append(result, frame)
			continue
		}
		if data, err := dibPNG(frame); err == nil {
			result = append(result, data)
		}
	}
	return result
}

// dibPNG decodes an icon bitmap: a BITMAPINFOHEADER, an optional palette,
// bottom-up colour rows and, when the header's height is doubled, a one-bit
// transparency mask.
func dibPNG(frame []byte) ([]byte, error) {
	if len(frame) < 40 {
		return nil, errors.New("bitmap header is short")
	}
	headerSize := int(binary.LittleEndian.Uint32(frame))
	// Icon bitmaps are bottom-up, so negative dimensions read as oversized and fail the bounds.
	width, height := int(binary.LittleEndian.Uint32(frame[4:])), int(binary.LittleEndian.Uint32(frame[8:]))
	depth := int(binary.LittleEndian.Uint16(frame[14:]))
	compression := binary.LittleEndian.Uint32(frame[16:])
	colours := int(binary.LittleEndian.Uint32(frame[32:]))
	if headerSize < 40 || headerSize > len(frame) || compression != 0 {
		return nil, errors.New("unsupported bitmap encoding")
	}
	if width == 0 || width > maxEdge || height == 0 || height > 2*maxEdge {
		return nil, fmt.Errorf("bitmap of %dx%d pixels", width, height)
	}
	rows, masked := height, false
	if height == 2*width {
		rows, masked = width, true
	}
	var palette []color.NRGBA
	pixels := headerSize
	switch depth {
	case 1, 4, 8:
		if colours == 0 || colours > 1<<depth {
			colours = 1 << depth
		}
		if len(frame) < pixels+4*colours {
			return nil, errors.New("bitmap palette exceeds the frame")
		}
		for i := range colours {
			entry := frame[pixels+4*i:]
			palette = append(palette, color.NRGBA{R: entry[2], G: entry[1], B: entry[0], A: 0xff})
		}
		pixels += 4 * colours
	case 24, 32:
	default:
		return nil, fmt.Errorf("unsupported bitmap depth %d", depth)
	}
	stride, maskStride := (width*depth+31)/32*4, (width+31)/32*4
	mask := pixels + stride*rows
	if len(frame) < mask {
		return nil, errors.New("bitmap pixels exceed the frame")
	}
	if masked && len(frame) < mask+maskStride*rows {
		masked = false
	}
	// Older 32-bit icons leave every alpha byte zero and rely on the mask.
	alpha := false
	for y := 0; depth == 32 && !alpha && y < rows; y++ {
		for x := range width {
			if frame[pixels+y*stride+4*x+3] != 0 {
				alpha = true
				break
			}
		}
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, rows))
	for y := range rows {
		source := rows - 1 - y
		row := frame[pixels+source*stride:]
		for x := range width {
			var pixel color.NRGBA
			switch depth {
			case 32:
				pixel = color.NRGBA{R: row[4*x+2], G: row[4*x+1], B: row[4*x], A: 0xff}
				if alpha {
					pixel.A = row[4*x+3]
				}
			case 24:
				pixel = color.NRGBA{R: row[3*x+2], G: row[3*x+1], B: row[3*x], A: 0xff}
			default:
				index := int(row[x*depth/8]>>(8-depth-x*depth%8)) & (1<<depth - 1)
				if index >= len(palette) {
					return nil, errors.New("bitmap pixel outside the palette")
				}
				pixel = palette[index]
			}
			if masked && !alpha && frame[mask+source*maskStride+x/8]>>(7-x%8)&1 == 1 {
				pixel.A = 0
			}
			img.SetNRGBA(x, y, pixel)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
