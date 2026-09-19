package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// testImage draws flat quadrants with one transparent corner, so decoders can
// be checked pixel by pixel and palettes stay small.
func testImage(edge int, shade uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, edge, edge))
	for y := range edge {
		for x := range edge {
			pixel := color.NRGBA{R: shade, G: 0x40, B: 0xc0, A: 0xff}
			switch {
			case x < edge/2 && y < edge/2:
				pixel = color.NRGBA{}
			case x >= edge/2:
				pixel = color.NRGBA{R: 0x10, G: shade, B: 0x20, A: 0xff}
			}
			img.SetNRGBA(x, y, pixel)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testPNG(t *testing.T, edge int, shade uint8) []byte {
	t.Helper()
	return encodePNG(t, testImage(edge, shade))
}

func decodePNG(t *testing.T, data []byte) *image.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	result := image.NewNRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			result.Set(x, y, img.At(x, y))
		}
	}
	return result
}

func testICNS(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("icns")
	total := 8
	for _, data := range entries {
		total += 8 + len(data)
	}
	_ = binary.Write(&buf, binary.BigEndian, uint32(total))
	for kind, data := range entries {
		buf.WriteString(kind)
		_ = binary.Write(&buf, binary.BigEndian, uint32(8+len(data)))
		buf.Write(data)
	}
	return buf.Bytes()
}

func testICO(frames ...[]byte) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, []uint16{0, 1, uint16(len(frames))})
	offset := 6 + 16*len(frames)
	for _, frame := range frames {
		buf.Write([]byte{0, 0, 0, 0})
		_ = binary.Write(&buf, binary.LittleEndian, []uint16{1, 32})
		_ = binary.Write(&buf, binary.LittleEndian, []uint32{uint32(len(frame)), uint32(offset)})
		offset += len(frame)
	}
	for _, frame := range frames {
		buf.Write(frame)
	}
	return buf.Bytes()
}

// testDIB encodes an icon bitmap at depth 8, 24 or 32 with a transparency
// mask; alpha selects whether 32-bit pixels carry their own alpha.
func testDIB(img *image.NRGBA, depth int, alpha bool) []byte {
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	var palette []color.NRGBA
	index := map[color.NRGBA]int{}
	if depth == 8 {
		for y := range height {
			for x := range width {
				pixel := img.NRGBAAt(x, y)
				pixel.A = 0xff
				if _, ok := index[pixel]; !ok {
					index[pixel] = len(palette)
					palette = append(palette, pixel)
				}
			}
		}
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, []uint32{40, uint32(width), uint32(2 * height)})
	_ = binary.Write(&buf, binary.LittleEndian, []uint16{1, uint16(depth)})
	_ = binary.Write(&buf, binary.LittleEndian, []uint32{0, 0, 0, 0, uint32(len(palette)), 0})
	for _, entry := range palette {
		buf.Write([]byte{entry.B, entry.G, entry.R, 0})
	}
	stride, maskStride := (width*depth+31)/32*4, (width+31)/32*4
	for y := height - 1; y >= 0; y-- {
		row := make([]byte, stride)
		for x := range width {
			pixel := img.NRGBAAt(x, y)
			switch depth {
			case 32:
				row[4*x], row[4*x+1], row[4*x+2] = pixel.B, pixel.G, pixel.R
				if alpha {
					row[4*x+3] = pixel.A
				}
			case 24:
				row[3*x], row[3*x+1], row[3*x+2] = pixel.B, pixel.G, pixel.R
			case 8:
				pixel.A = 0xff
				row[x] = byte(index[pixel])
			}
		}
		buf.Write(row)
	}
	for y := height - 1; y >= 0; y-- {
		row := make([]byte, maskStride)
		for x := range width {
			if img.NRGBAAt(x, y).A == 0 {
				row[x/8] |= 1 << (7 - x%8)
			}
		}
		buf.Write(row)
	}
	return buf.Bytes()
}

// testPE builds a PE32+ image whose only section holds one icon group made of
// frames; no frames means no resource directory at all.
func testPE(resourceOffset uint32, frames ...[]byte) []byte {
	const sectionAddress, rawOffset = 0x1000, 0x200
	var rsrc []byte
	if len(frames) > 0 {
		rsrc = append(make([]byte, resourceOffset), testResources(frames, sectionAddress+resourceOffset)...)
	}
	image := make([]byte, rawOffset+len(rsrc))
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[0x3c:], 0x40)
	copy(image[0x40:], "PE\x00\x00")
	coff := image[0x44:]
	binary.LittleEndian.PutUint16(coff, 0x8664)
	binary.LittleEndian.PutUint16(coff[2:], 1)
	binary.LittleEndian.PutUint16(coff[16:], 240)
	binary.LittleEndian.PutUint16(coff[18:], 0x22)
	optional := coff[20:]
	binary.LittleEndian.PutUint16(optional, 0x20b)
	binary.LittleEndian.PutUint32(optional[108:], 16)
	if len(rsrc) > 0 {
		binary.LittleEndian.PutUint32(optional[112+8*resourceDirectory:], sectionAddress+resourceOffset)
		binary.LittleEndian.PutUint32(optional[112+8*resourceDirectory+4:], uint32(len(rsrc))-resourceOffset)
	}
	section := optional[240:]
	copy(section, ".rsrc")
	binary.LittleEndian.PutUint32(section[8:], uint32(max(len(rsrc), 1)))
	binary.LittleEndian.PutUint32(section[12:], sectionAddress)
	binary.LittleEndian.PutUint32(section[16:], uint32(len(rsrc)))
	binary.LittleEndian.PutUint32(section[20:], rawOffset)
	binary.LittleEndian.PutUint32(section[36:], 0x40000040)
	copy(image[rawOffset:], rsrc)
	return image
}

func testResources(frames [][]byte, address uint32) []byte {
	n := len(frames)
	directory := func(buf *bytes.Buffer, entries ...[2]uint32) {
		buf.Write(make([]byte, 12))
		_ = binary.Write(buf, binary.LittleEndian, []uint16{0, uint16(len(entries))})
		for _, entry := range entries {
			_ = binary.Write(buf, binary.LittleEndian, entry[:])
		}
	}
	subdirectory := func(offset int) uint32 { return uint32(offset) | 1<<31 }
	typeIcons, typeGroups := 32, 48+8*n
	language := func(i int) int { return 72 + 8*n + 24*i } // Icons, then the group.
	dataEntry := func(i int) int { return 96 + 32*n + 16*i }
	blobs := 112 + 48*n
	var buf bytes.Buffer
	directory(&buf, [2]uint32{resourceIcon, subdirectory(typeIcons)}, [2]uint32{resourceIconGroup, subdirectory(typeGroups)})
	icons := make([][2]uint32, 0, n)
	for i := range n {
		icons = append(icons, [2]uint32{uint32(i + 1), subdirectory(language(i))})
	}
	directory(&buf, icons...)
	directory(&buf, [2]uint32{1, subdirectory(language(n))})
	for i := range n + 1 {
		directory(&buf, [2]uint32{0x409, uint32(dataEntry(i))})
	}
	var group bytes.Buffer
	_ = binary.Write(&group, binary.LittleEndian, []uint16{0, 1, uint16(n)})
	for i, frame := range frames {
		group.Write([]byte{0, 0, 0, 0})
		_ = binary.Write(&group, binary.LittleEndian, []uint16{1, 32})
		_ = binary.Write(&group, binary.LittleEndian, uint32(len(frame)))
		_ = binary.Write(&group, binary.LittleEndian, uint16(i+1))
	}
	offset := blobs + group.Len()
	for _, frame := range frames {
		_ = binary.Write(&buf, binary.LittleEndian, []uint32{address + uint32(offset), uint32(len(frame)), 0, 0})
		offset += len(frame)
	}
	_ = binary.Write(&buf, binary.LittleEndian, []uint32{address + uint32(blobs), uint32(group.Len()), 0, 0})
	if buf.Len() != blobs {
		panic("resource layout mismatch")
	}
	buf.Write(group.Bytes())
	for _, frame := range frames {
		buf.Write(frame)
	}
	return buf.Bytes()
}
