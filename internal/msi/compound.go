// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

package msi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf16"
)

const (
	cfbEndOfChain = uint32(0xfffffffe)
	cfbFreeSect   = uint32(0xffffffff)
	cfbFatSect    = uint32(0xfffffffd)
	cfbDifSect    = uint32(0xfffffffc)

	cfbDirectoryEntrySize = 128
	cfbMaxChainSectors    = 1_000_000
)

var cfbSignature = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

type directoryEntry struct {
	name        string
	objectType  byte
	startSector uint32
	size        int64
}

type compoundFile struct {
	data           []byte
	sectorSize     int64
	miniSectorSize int64
	miniCutoff     int64
	fat            []uint32
	miniFat        []uint32
	miniStream     []byte
	streams        []directoryEntry
}

func newCompoundFile(data []byte) (*compoundFile, error) {
	if len(data) < 512 || !bytes.Equal(data[:8], cfbSignature) {
		return nil, errors.New("bad compound file signature")
	}
	if binary.LittleEndian.Uint16(data[28:30]) != 0xfffe {
		return nil, errors.New("unsupported compound file byte order")
	}

	sectorShift, ok := u16(data, 30)
	if !ok {
		return nil, errors.New("truncated compound file header")
	}
	miniSectorShift, ok := u16(data, 32)
	if !ok {
		return nil, errors.New("truncated compound file header")
	}
	if sectorShift != 9 && sectorShift != 12 {
		return nil, fmt.Errorf("unsupported sector shift %d", sectorShift)
	}
	if miniSectorShift != 6 {
		return nil, fmt.Errorf("unsupported mini sector shift %d", miniSectorShift)
	}

	cf := &compoundFile{
		data:           data,
		sectorSize:     int64(1) << sectorShift,
		miniSectorSize: int64(1) << miniSectorShift,
	}
	if int64(len(data)) < cf.sectorSize {
		return nil, errors.New("compound file is smaller than its header sector")
	}

	firstDirSector, _ := u32(data, 48)
	miniCutoff, _ := u32(data, 56)
	firstMiniFatSector, _ := u32(data, 60)
	miniFatSectorCount, _ := u32(data, 64)
	firstDifatSector, _ := u32(data, 68)
	difatSectorCount, _ := u32(data, 72)
	cf.miniCutoff = int64(miniCutoff)

	fatSectors, err := cf.readDifat(firstDifatSector, difatSectorCount)
	if err != nil {
		return nil, err
	}
	cf.fat, err = cf.readFat(fatSectors)
	if err != nil {
		return nil, err
	}

	if firstMiniFatSector != cfbEndOfChain && firstMiniFatSector != cfbFreeSect && miniFatSectorCount > 0 {
		miniFatBytes, err := cf.readRegularChain(firstMiniFatSector, int64(miniFatSectorCount)*cf.sectorSize)
		if err != nil {
			return nil, err
		}
		cf.miniFat = uint32s(miniFatBytes)
	}

	dirBytes, err := cf.readRegularChain(firstDirSector, -1)
	if err != nil {
		return nil, err
	}
	root, streams, err := parseDirectory(dirBytes, cf.sectorSize == 512)
	if err != nil {
		return nil, err
	}
	cf.streams = streams

	if root != nil && root.size > 0 && root.startSector != cfbEndOfChain && root.startSector != cfbFreeSect {
		cf.miniStream, err = cf.readRegularChain(root.startSector, root.size)
		if err != nil {
			return nil, err
		}
	}

	return cf, nil
}

func (cf *compoundFile) readStream(entry directoryEntry) ([]byte, error) {
	if entry.size < 0 {
		return nil, errors.New("stream has negative size")
	}
	if entry.size < cf.miniCutoff {
		return cf.readMiniChain(entry.startSector, entry.size)
	}
	return cf.readRegularChain(entry.startSector, entry.size)
}

func (cf *compoundFile) readDifat(firstDifatSector uint32, difatSectorCount uint32) ([]uint32, error) {
	if difatSectorCount > 4096 {
		return nil, errors.New("DIFAT exceeds supported limit")
	}
	entriesPerSector := int(cf.sectorSize / 4)
	fatSectors := make([]uint32, 0)
	for i := range 109 {
		sector, ok := u32(cf.data, 76+i*4)
		if !ok {
			return nil, errors.New("truncated DIFAT header")
		}
		if sector != cfbFreeSect && sector != cfbEndOfChain {
			fatSectors = append(fatSectors, sector)
		}
	}

	sector := firstDifatSector
	seen := map[uint32]bool{}
	for i := uint32(0); sector != cfbEndOfChain && sector != cfbFreeSect && i < difatSectorCount; i++ {
		if seen[sector] {
			return nil, errors.New("cycle in DIFAT chain")
		}
		seen[sector] = true

		off, err := cf.sectorOffset(sector)
		if err != nil {
			return nil, err
		}
		for j := 0; j < entriesPerSector-1; j++ {
			fatSector, ok := u32(cf.data, off+j*4)
			if !ok {
				return nil, errors.New("truncated DIFAT sector")
			}
			if fatSector != cfbFreeSect && fatSector != cfbEndOfChain {
				fatSectors = append(fatSectors, fatSector)
			}
		}
		next, ok := u32(cf.data, off+(entriesPerSector-1)*4)
		if !ok {
			return nil, errors.New("truncated DIFAT next sector")
		}
		sector = next
	}
	if sector != cfbEndOfChain && sector != cfbFreeSect {
		return nil, errors.New("DIFAT chain exceeds declared sector count")
	}
	return fatSectors, nil
}

func (cf *compoundFile) readFat(fatSectors []uint32) ([]uint32, error) {
	if len(fatSectors) > len(cf.data)/int(cf.sectorSize) {
		return nil, errors.New("FAT exceeds file size")
	}
	entriesPerSector := int(cf.sectorSize / 4)
	fat := make([]uint32, 0, len(fatSectors)*entriesPerSector)
	seen := make(map[uint32]bool, len(fatSectors))
	for _, sector := range fatSectors {
		if seen[sector] {
			return nil, errors.New("duplicate FAT sector")
		}
		seen[sector] = true
		off, err := cf.sectorOffset(sector)
		if err != nil {
			return nil, err
		}
		for i := range entriesPerSector {
			v, ok := u32(cf.data, off+i*4)
			if !ok {
				return nil, errors.New("truncated FAT sector")
			}
			fat = append(fat, v)
		}
	}
	return fat, nil
}

func (cf *compoundFile) readRegularChain(start uint32, size int64) ([]byte, error) {
	return readChain(start, size, cf.sectorSize, cf.fat, func(sector uint32) (int, error) {
		return cf.sectorOffset(sector)
	}, cf.data)
}

func (cf *compoundFile) readMiniChain(start uint32, size int64) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	return readChain(start, size, cf.miniSectorSize, cf.miniFat, func(sector uint32) (int, error) {
		off := int64(sector) * cf.miniSectorSize
		if off < 0 || off+cf.miniSectorSize > int64(len(cf.miniStream)) {
			return 0, errors.New("mini stream sector is out of bounds")
		}
		return int(off), nil
	}, cf.miniStream)
}

func readChain(
	start uint32,
	size int64,
	sectorSize int64,
	fat []uint32,
	offset func(uint32) (int, error),
	source []byte,
) ([]byte, error) {
	if size > 32<<20 || size > int64(len(source)) {
		return nil, errors.New("metadata stream exceeds supported size")
	}
	if size == 0 {
		return nil, nil
	}
	if start == cfbEndOfChain || start == cfbFreeSect {
		return nil, errors.New("stream starts at an invalid sector")
	}
	if sectorSize <= 0 || sectorSize > math.MaxInt32 {
		return nil, errors.New("invalid sector size")
	}

	var out []byte
	if size > 0 && size <= int64(math.MaxInt32) {
		out = make([]byte, 0, size)
	}
	seen := map[uint32]bool{}
	for sector := start; sector != cfbEndOfChain && sector != cfbFreeSect; {
		if int(sector) >= len(fat) {
			return nil, errors.New("sector chain points outside the FAT")
		}
		if seen[sector] {
			return nil, errors.New("cycle in sector chain")
		}
		if len(seen) >= cfbMaxChainSectors {
			return nil, errors.New("sector chain is too long")
		}
		seen[sector] = true

		off, err := offset(sector)
		if err != nil {
			return nil, err
		}
		end := int64(off) + sectorSize
		if off < 0 || end > int64(len(source)) {
			return nil, errors.New("sector is out of bounds")
		}
		out = append(out, source[off:int(end)]...)
		if len(out) > 32<<20 {
			return nil, errors.New("metadata stream exceeds supported size")
		}
		if size >= 0 && int64(len(out)) >= size {
			return out[:size], nil
		}

		next := fat[sector]
		if next == cfbFatSect || next == cfbDifSect {
			return nil, errors.New("sector chain points at a reserved sector")
		}
		sector = next
	}
	if size >= 0 && int64(len(out)) < size {
		return nil, errors.New("sector chain ended before stream size")
	}
	return out, nil
}

func (cf *compoundFile) sectorOffset(sector uint32) (int, error) {
	off := (int64(sector) + 1) * cf.sectorSize
	if off < 0 || off+cf.sectorSize > int64(len(cf.data)) {
		return 0, errors.New("sector offset is out of bounds")
	}
	if off > int64(math.MaxInt32) {
		return 0, errors.New("sector offset is too large")
	}
	return int(off), nil
}

func parseDirectory(data []byte, v3 bool) (*directoryEntry, []directoryEntry, error) {
	if len(data)/cfbDirectoryEntrySize > 8192 {
		return nil, nil, errors.New("too many compound directory entries")
	}
	var root *directoryEntry
	var streams []directoryEntry
	for off := 0; off+cfbDirectoryEntrySize <= len(data); off += cfbDirectoryEntrySize {
		objectType := data[off+66]
		if objectType != 1 && objectType != 2 && objectType != 5 {
			continue
		}

		nameLen := int(binary.LittleEndian.Uint16(data[off+64 : off+66]))
		if nameLen < 2 || nameLen > 64 || nameLen%2 != 0 {
			return nil, nil, errors.New("invalid directory entry name length")
		}
		name := utf16String(data[off : off+nameLen-2])
		size64 := binary.LittleEndian.Uint64(data[off+120 : off+128])
		if size64 > math.MaxInt64 {
			return nil, nil, errors.New("directory entry stream size is too large")
		}
		size := int64(size64)
		if v3 {
			size = int64(binary.LittleEndian.Uint32(data[off+120 : off+124]))
		}
		entry := directoryEntry{
			name:        name,
			objectType:  objectType,
			startSector: binary.LittleEndian.Uint32(data[off+116 : off+120]),
			size:        size,
		}
		switch objectType {
		case 5:
			rootEntry := entry
			root = &rootEntry
		case 2:
			streams = append(streams, entry)
		}
	}
	if root == nil {
		return nil, nil, errors.New("compound file has no root storage")
	}
	return root, streams, nil
}

func utf16String(data []byte) string {
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(data[i:i+2]))
	}
	return string(utf16.Decode(units))
}

func uint32s(data []byte) []uint32 {
	values := make([]uint32, len(data)/4)
	for i := range values {
		values[i] = binary.LittleEndian.Uint32(data[i*4 : i*4+4])
	}
	return values
}

func u16(data []byte, off int) (uint16, bool) {
	if off < 0 || off+2 > len(data) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(data[off : off+2]), true
}

func u32(data []byte, off int) (uint32, bool) {
	if off < 0 || off+4 > len(data) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(data[off : off+4]), true
}
