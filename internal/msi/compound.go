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
	"io"
	"math"
	"unicode/utf16"
)

const (
	cfbEndOfChain = uint32(0xfffffffe)
	cfbFreeSect   = uint32(0xffffffff)
	cfbFatSect    = uint32(0xfffffffd)
	cfbDifSect    = uint32(0xfffffffc)

	cfbHeaderSize         = 512
	cfbDirectoryEntrySize = 128
	cfbMaxChainSectors    = 1_000_000
	cfbMaxStreamSize      = 32 << 20
)

var cfbSignature = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

type directoryEntry struct {
	name        string
	objectType  byte
	startSector uint32
	size        int64
}

// compoundFile reads metadata streams by random access. Cabinet streams are
// never read, so memory follows the metadata rather than the installer size.
type compoundFile struct {
	file           io.ReaderAt
	size           int64
	sectorSize     int64
	miniSectorSize int64
	miniCutoff     int64
	fatSectors     []uint32
	// fat caches the FAT sector read last; chains are mostly contiguous, so
	// consecutive links usually share it.
	fat struct {
		index   int
		entries []uint32
	}
	miniFat    []uint32
	miniStream []byte
	streams    []directoryEntry
}

func newCompoundFile(file io.ReaderAt, size int64) (*compoundFile, error) {
	if size < cfbHeaderSize {
		return nil, errors.New("bad compound file signature")
	}
	header := make([]byte, cfbHeaderSize)
	if _, err := file.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("read compound file header: %w", err)
	}
	if !bytes.Equal(header[:8], cfbSignature) {
		return nil, errors.New("bad compound file signature")
	}
	if binary.LittleEndian.Uint16(header[28:30]) != 0xfffe {
		return nil, errors.New("unsupported compound file byte order")
	}

	sectorShift := binary.LittleEndian.Uint16(header[30:32])
	miniSectorShift := binary.LittleEndian.Uint16(header[32:34])
	if sectorShift != 9 && sectorShift != 12 {
		return nil, fmt.Errorf("unsupported sector shift %d", sectorShift)
	}
	if miniSectorShift != 6 {
		return nil, fmt.Errorf("unsupported mini sector shift %d", miniSectorShift)
	}

	cf := &compoundFile{
		file:           file,
		size:           size,
		sectorSize:     int64(1) << sectorShift,
		miniSectorSize: int64(1) << miniSectorShift,
	}
	cf.fat.index = -1
	if size < cf.sectorSize {
		return nil, errors.New("compound file is smaller than its header sector")
	}

	firstDirSector := binary.LittleEndian.Uint32(header[48:52])
	cf.miniCutoff = int64(binary.LittleEndian.Uint32(header[56:60]))
	firstMiniFatSector := binary.LittleEndian.Uint32(header[60:64])
	miniFatSectorCount := binary.LittleEndian.Uint32(header[64:68])
	firstDifatSector := binary.LittleEndian.Uint32(header[68:72])
	difatSectorCount := binary.LittleEndian.Uint32(header[72:76])

	var err error
	cf.fatSectors, err = cf.readDifat(header, firstDifatSector, difatSectorCount)
	if err != nil {
		return nil, err
	}
	if int64(len(cf.fatSectors)) > size/cf.sectorSize {
		return nil, errors.New("FAT exceeds file size")
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

func (cf *compoundFile) readDifat(header []byte, firstDifatSector uint32, difatSectorCount uint32) ([]uint32, error) {
	if difatSectorCount > 4096 {
		return nil, errors.New("DIFAT exceeds supported limit")
	}
	entriesPerSector := int(cf.sectorSize / 4)
	fatSectors := make([]uint32, 0)
	for i := range 109 {
		sector := binary.LittleEndian.Uint32(header[76+i*4:])
		if sector != cfbFreeSect && sector != cfbEndOfChain {
			fatSectors = append(fatSectors, sector)
		}
	}

	sector := firstDifatSector
	seen := map[uint32]bool{}
	buf := make([]byte, cf.sectorSize)
	for i := uint32(0); sector != cfbEndOfChain && sector != cfbFreeSect && i < difatSectorCount; i++ {
		if seen[sector] {
			return nil, errors.New("cycle in DIFAT chain")
		}
		seen[sector] = true
		if err := cf.readSector(sector, buf); err != nil {
			return nil, err
		}
		for j := range entriesPerSector - 1 {
			fatSector := binary.LittleEndian.Uint32(buf[j*4:])
			if fatSector != cfbFreeSect && fatSector != cfbEndOfChain {
				fatSectors = append(fatSectors, fatSector)
			}
		}
		sector = binary.LittleEndian.Uint32(buf[(entriesPerSector-1)*4:])
	}
	if sector != cfbEndOfChain && sector != cfbFreeSect {
		return nil, errors.New("DIFAT chain exceeds declared sector count")
	}
	return fatSectors, nil
}

// next returns the FAT entry linking sector to the one that follows it.
func (cf *compoundFile) next(sector uint32) (uint32, error) {
	entriesPerSector := int(cf.sectorSize / 4)
	index := int(sector) / entriesPerSector
	if index >= len(cf.fatSectors) {
		return 0, errors.New("sector chain points outside the FAT")
	}
	if cf.fat.index != index {
		buf := make([]byte, cf.sectorSize)
		if err := cf.readSector(cf.fatSectors[index], buf); err != nil {
			return 0, err
		}
		cf.fat.index, cf.fat.entries = index, uint32s(buf)
	}
	return cf.fat.entries[int(sector)%entriesPerSector], nil
}

func (cf *compoundFile) readRegularChain(start uint32, size int64) ([]byte, error) {
	return readChain(start, size, cf.sectorSize, cf.size, cf.next, cf.readSector)
}

func (cf *compoundFile) readMiniChain(start uint32, size int64) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	next := func(sector uint32) (uint32, error) {
		if int(sector) >= len(cf.miniFat) {
			return 0, errors.New("sector chain points outside the FAT")
		}
		return cf.miniFat[sector], nil
	}
	read := func(sector uint32, buf []byte) error {
		off := int64(sector) * cf.miniSectorSize
		if off < 0 || off+cf.miniSectorSize > int64(len(cf.miniStream)) {
			return errors.New("mini stream sector is out of bounds")
		}
		copy(buf, cf.miniStream[off:off+cf.miniSectorSize])
		return nil
	}
	return readChain(start, size, cf.miniSectorSize, int64(len(cf.miniStream)), next, read)
}

// readChain follows a sector chain from start and returns size bytes, or the
// whole chain when size is negative. No stream is larger than limit, the size
// of the store holding it.
func readChain(
	start uint32,
	size int64,
	sectorSize int64,
	limit int64,
	next func(uint32) (uint32, error),
	read func(uint32, []byte) error,
) ([]byte, error) {
	if size > cfbMaxStreamSize || size > limit {
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
	if size > 0 {
		out = make([]byte, 0, size)
	}
	buf := make([]byte, sectorSize)
	seen := map[uint32]bool{}
	for sector := start; sector != cfbEndOfChain && sector != cfbFreeSect; {
		if seen[sector] {
			return nil, errors.New("cycle in sector chain")
		}
		if len(seen) >= cfbMaxChainSectors {
			return nil, errors.New("sector chain is too long")
		}
		seen[sector] = true

		link, err := next(sector)
		if err != nil {
			return nil, err
		}
		if err := read(sector, buf); err != nil {
			return nil, err
		}
		out = append(out, buf...)
		if len(out) > cfbMaxStreamSize {
			return nil, errors.New("metadata stream exceeds supported size")
		}
		if size >= 0 && int64(len(out)) >= size {
			return out[:size], nil
		}

		if link == cfbFatSect || link == cfbDifSect {
			return nil, errors.New("sector chain points at a reserved sector")
		}
		sector = link
	}
	if size >= 0 && int64(len(out)) < size {
		return nil, errors.New("sector chain ended before stream size")
	}
	return out, nil
}

// readSector fills buf with a regular sector. Sector 0 follows the header,
// which occupies one sector.
func (cf *compoundFile) readSector(sector uint32, buf []byte) error {
	off := (int64(sector) + 1) * cf.sectorSize
	if off+cf.sectorSize > cf.size {
		return errors.New("sector offset is out of bounds")
	}
	if _, err := cf.file.ReadAt(buf[:cf.sectorSize], off); err != nil {
		return fmt.Errorf("read compound file sector: %w", err)
	}
	return nil
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
