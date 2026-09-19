package icon

import (
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	resourceIcon      = 3
	resourceIconGroup = 14
	resourceDirectory = 2
	maxResourceBytes  = 64 << 20
)

// FromPE returns the largest frame of the first icon group in a Windows
// executable's resources, the icon Explorer shows for the file.
func FromPE(r io.ReaderAt) ([]byte, error) {
	file, err := pe.NewFile(r)
	if err != nil {
		return nil, fmt.Errorf("executable: %w", err)
	}
	defer func() { _ = file.Close() }()
	var directory pe.DataDirectory
	switch header := file.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		if len(header.DataDirectory) > resourceDirectory {
			directory = header.DataDirectory[resourceDirectory]
		}
	case *pe.OptionalHeader64:
		if len(header.DataDirectory) > resourceDirectory {
			directory = header.DataDirectory[resourceDirectory]
		}
	}
	if directory.VirtualAddress == 0 || directory.Size == 0 {
		return nil, ErrNoArtwork
	}
	var section *pe.Section
	for _, candidate := range file.Sections {
		if candidate.VirtualAddress <= directory.VirtualAddress && directory.VirtualAddress-candidate.VirtualAddress < candidate.VirtualSize {
			section = candidate
			break
		}
	}
	if section == nil || section.Size > maxResourceBytes {
		return nil, errors.New("executable resources are missing or too large")
	}
	data, err := section.Data()
	if err != nil {
		return nil, err
	}
	rs := resources{data: data, base: directory.VirtualAddress - section.VirtualAddress, address: section.VirtualAddress}
	groups, err := rs.entries(rs.base)
	if err != nil {
		return nil, err
	}
	group, ok := groups.subdirectory(resourceIconGroup)
	if !ok {
		return nil, ErrNoArtwork
	}
	header, err := rs.first(group)
	if err != nil {
		return nil, err
	}
	icons, ok := groups.subdirectory(resourceIcon)
	if !ok {
		return nil, errors.New("icon group without icon resources")
	}
	members, err := rs.entries(icons)
	if err != nil {
		return nil, err
	}
	if len(header) < 6 || binary.LittleEndian.Uint16(header[2:]) != 1 {
		return nil, errors.New("malformed icon group")
	}
	count := int(binary.LittleEndian.Uint16(header[4:]))
	if len(header) < 6+14*count {
		return nil, errors.New("icon group exceeds its resource")
	}
	frames := make([][]byte, 0, count)
	for i := range count {
		id := uint32(binary.LittleEndian.Uint16(header[6+14*i+12:]))
		member, ok := members.subdirectory(id)
		if !ok {
			return nil, fmt.Errorf("icon group names missing icon %d", id)
		}
		frame, err := rs.first(member)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}
	return largest(framesPNG(frames))
}

// resources walks the resource section: directories of entries whose high
// offset bit points at a subdirectory, otherwise at a data entry.
type resources struct {
	data    []byte
	base    uint32 // Directory root within data.
	address uint32 // Section virtual address, to resolve data RVAs.
}

type resourceEntry struct {
	id, offset uint32
	directory  bool
}

type resourceEntries []resourceEntry

func (rs resources) entries(offset uint32) (resourceEntries, error) {
	if uint64(offset)+16 > uint64(len(rs.data)) {
		return nil, errors.New("resource directory exceeds the section")
	}
	header := rs.data[offset:]
	count := uint64(binary.LittleEndian.Uint16(header[12:])) + uint64(binary.LittleEndian.Uint16(header[14:]))
	if uint64(offset)+16+8*count > uint64(len(rs.data)) {
		return nil, errors.New("resource entries exceed the section")
	}
	result := make(resourceEntries, 0, count)
	for i := range count {
		entry := header[16+8*i:]
		id, target := binary.LittleEndian.Uint32(entry), binary.LittleEndian.Uint32(entry[4:])
		offset := uint64(rs.base) + uint64(target&^(1<<31))
		if offset > math.MaxUint32 || offset+16 > uint64(len(rs.data)) {
			return nil, errors.New("resource target exceeds the section")
		}
		result = append(result, resourceEntry{id: id, offset: uint32(offset), directory: target&(1<<31) != 0})
	}
	return result, nil
}

// subdirectory finds the entry with a numeric id; named entries carry the high bit.
func (entries resourceEntries) subdirectory(id uint32) (uint32, bool) {
	for _, entry := range entries {
		if entry.id == id && entry.directory {
			return entry.offset, true
		}
	}
	return 0, false
}

// first returns the data of the first entry in a directory, resolving one
// more directory level when the entry holds languages.
func (rs resources) first(offset uint32) ([]byte, error) {
	entries, err := rs.entries(offset)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("empty resource directory")
	}
	if entries[0].directory {
		return rs.first(entries[0].offset)
	}
	if uint64(entries[0].offset)+16 > uint64(len(rs.data)) {
		return nil, errors.New("resource data entry exceeds the section")
	}
	entry := rs.data[entries[0].offset:]
	address, size := binary.LittleEndian.Uint32(entry), binary.LittleEndian.Uint32(entry[4:])
	if address < rs.address || uint64(address-rs.address)+uint64(size) > uint64(len(rs.data)) {
		return nil, errors.New("resource data exceeds the section")
	}
	start := address - rs.address
	return rs.data[start : start+size], nil
}
