package apple

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // CodeDirectory hash type 1 is SHA-1.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"sort"
)

const maxSignature = 64 << 20

// MachOFacts reports claimed CodeDirectory values without authenticating them.
type MachOFacts struct {
	Architectures []Architecture `json:"architectures"`
}

// Architecture describes one independently signed Mach-O slice.
type Architecture struct {
	CPU                 uint32 `json:"cpu"`
	Identifier          string `json:"identifier"`
	TeamID              string `json:"team_id,omitempty"`
	AdHoc               bool   `json:"ad_hoc"`
	HasCMS              bool   `json:"has_cms"`
	CodeDirectorySHA256 string `json:"code_directory_sha256"`
}

type machoSlice struct {
	r    io.ReaderAt
	size int64
	cpu  uint32
}

type codeSignature struct {
	codeOffset  int64
	blobs       map[uint32][]byte
	directories [][]byte
}

// InspectMachO reads all architecture signatures without treating their claimed
// identifiers, Team IDs or recorded code hashes as proof of identity.
func InspectMachO(filePath string) (MachOFacts, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return MachOFacts{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return MachOFacts{}, err
	}
	slices, err := machoSlices(f, info.Size())
	if err != nil {
		return MachOFacts{}, err
	}
	var facts MachOFacts
	for _, slice := range slices {
		signature, err := slice.signature()
		if err != nil {
			return facts, err
		}
		cd := signature.directories[0]
		if err := validateCodeDirectory(cd); err != nil {
			return facts, err
		}
		h := sha256.Sum256(cd)
		entry := Architecture{CPU: slice.cpu, AdHoc: binary.BigEndian.Uint32(cd[12:16])&2 != 0,
			HasCMS: len(signature.blobs[0x10000]) > 8, CodeDirectorySHA256: hex.EncodeToString(h[:])}
		entry.Identifier, err = codeString(cd, binary.BigEndian.Uint32(cd[20:24]))
		if err != nil {
			return facts, err
		}
		if binary.BigEndian.Uint32(cd[8:12]) >= 0x20200 {
			teamOffset := binary.BigEndian.Uint32(cd[48:52])
			if teamOffset != 0 {
				entry.TeamID, err = codeString(cd, teamOffset)
				if err != nil {
					return facts, err
				}
			}
		}
		facts.Architectures = append(facts.Architectures, entry)
	}
	return facts, nil
}

// verifyMachO authenticates every architecture of a signed Mach-O: each
// CodeDirectory seals its code pages and special slots, its CMS signature
// chains to Apple at the signature's trusted time, and all architectures
// share one signer and identifier.
func verifyMachO(ctx context.Context, r io.ReaderAt, size int64, external map[uint32][]byte) (codeIdentity, error) {
	slices, err := machoSlices(contextReaderAt{ctx, r}, size)
	if err != nil {
		return codeIdentity{}, err
	}
	var identity codeIdentity
	for i, slice := range slices {
		if err := ctx.Err(); err != nil {
			return codeIdentity{}, err
		}
		current, err := slice.verify(external)
		if err != nil {
			return codeIdentity{}, fmt.Errorf("Mach-O CPU %#x: %w", slice.cpu, err)
		}
		if i == 0 {
			identity = current
			continue
		}
		if !identity.same(current) {
			return codeIdentity{}, fmt.Errorf("Mach-O architectures are signed by different identities")
		}
		identity.cdhashes = append(identity.cdhashes, current.cdhashes...)
	}
	return identity, ctx.Err()
}

func (m machoSlice) verify(external map[uint32][]byte) (codeIdentity, error) {
	sig, err := m.signature()
	if err != nil {
		return codeIdentity{}, err
	}
	if external[1] == nil {
		embedded, err := m.embeddedInfoPlist()
		if err != nil {
			return codeIdentity{}, err
		}
		if embedded != nil {
			external = maps.Clone(external)
			if external == nil {
				external = map[uint32][]byte{}
			}
			external[1] = embedded
		}
	}
	for _, cd := range sig.directories {
		if err := m.verifyCodeDirectory(cd, sig, external); err != nil {
			return codeIdentity{}, err
		}
	}
	cms, err := verifyCMS(sig)
	if err != nil {
		return codeIdentity{}, err
	}
	at, err := trustedTime(cms.timestampToken, cms.signatureValue)
	if err != nil {
		return codeIdentity{}, err
	}
	identity, err := identify(cms.certificate, cms.certificates, at)
	if err != nil {
		return codeIdentity{}, err
	}
	if identity.identifier, err = codeString(sig.directories[0], binary.BigEndian.Uint32(sig.directories[0][20:24])); err != nil {
		return codeIdentity{}, err
	}
	for _, cd := range sig.directories {
		h, _, err := codeHash(cd[37])
		if err != nil {
			return codeIdentity{}, err
		}
		_, _ = h.Write(cd)
		// A cdhash is the CodeDirectory hash truncated to 20 bytes.
		identity.cdhashes = append(identity.cdhashes, h.Sum(nil)[:20])
	}
	return identity, nil
}

func machoSlices(r io.ReaderAt, size int64) ([]machoSlice, error) {
	var header [8]byte
	if size < 8 {
		return nil, fmt.Errorf("truncated Mach-O header")
	}
	if _, err := r.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	magic := binary.BigEndian.Uint32(header[:4])
	if magic != 0xcafebabe && magic != 0xcafebabf {
		order, _, err := machoOrder(magic)
		if err != nil {
			return nil, err
		}
		return []machoSlice{{r: r, size: size, cpu: order.Uint32(header[4:8])}}, nil
	}
	count := binary.BigEndian.Uint32(header[4:8])
	if count == 0 || count > 32 {
		return nil, fmt.Errorf("invalid Mach-O architecture count")
	}
	entrySize := uint64(20)
	if magic == 0xcafebabf {
		entrySize = 32
	}
	tableEnd := 8 + entrySize*uint64(count)
	if tableEnd > uint64(size) {
		return nil, fmt.Errorf("truncated universal Mach-O table")
	}
	table := make([]byte, entrySize*uint64(count))
	if _, err := r.ReadAt(table, 8); err != nil {
		return nil, err
	}
	var slices []machoSlice
	type span struct{ start, end uint64 }
	var spans []span
	for i := range count {
		entry := table[uint64(i)*entrySize : uint64(i+1)*entrySize]
		cpu := binary.BigEndian.Uint32(entry[:4])
		offset, length := uint64(binary.BigEndian.Uint32(entry[8:12])), uint64(binary.BigEndian.Uint32(entry[12:16]))
		align := binary.BigEndian.Uint32(entry[16:20])
		if entrySize == 32 {
			offset, length, align = binary.BigEndian.Uint64(entry[8:16]), binary.BigEndian.Uint64(entry[16:24]), binary.BigEndian.Uint32(entry[24:28])
		}
		if offset < tableEnd || length < 28 || offset > uint64(size) || length > uint64(size)-offset || align > 31 || offset%(uint64(1)<<align) != 0 {
			return nil, fmt.Errorf("invalid universal Mach-O slice range")
		}
		section := io.NewSectionReader(r, int64(offset), int64(length)) //nolint:gosec // The range check above bounds both by size.
		if _, err := section.ReadAt(header[:], 0); err != nil {
			return nil, err
		}
		order, _, err := machoOrder(binary.BigEndian.Uint32(header[:4]))
		if err != nil {
			return nil, err
		}
		if order.Uint32(header[4:8]) != cpu {
			return nil, fmt.Errorf("universal Mach-O CPU disagrees with slice")
		}
		slices = append(slices, machoSlice{r: section, size: section.Size(), cpu: cpu})
		spans = append(spans, span{offset, offset + length})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return nil, fmt.Errorf("overlapping universal Mach-O slices")
		}
	}
	return slices, nil
}

func machoOrder(magic uint32) (binary.ByteOrder, int64, error) {
	switch magic {
	case 0xfeedface:
		return binary.BigEndian, 28, nil
	case 0xfeedfacf:
		return binary.BigEndian, 32, nil
	case 0xcefaedfe:
		return binary.LittleEndian, 28, nil
	case 0xcffaedfe:
		return binary.LittleEndian, 32, nil
	default:
		return nil, 0, fmt.Errorf("%w: not a supported thin or universal Mach-O", ErrUnsupported)
	}
}

// loadCommands calls visit with each load command's kind and bytes.
func (m machoSlice) loadCommands(visit func(order binary.ByteOrder, kind uint32, command []byte) error) (int64, error) {
	var header [28]byte
	if _, err := m.r.ReadAt(header[:], 0); err != nil {
		return 0, err
	}
	order, headerSize, err := machoOrder(binary.BigEndian.Uint32(header[:4]))
	if err != nil {
		return 0, err
	}
	count, size := order.Uint32(header[16:20]), order.Uint32(header[20:24])
	if count > 65536 || size > 16<<20 || int64(size) > m.size-headerSize {
		return 0, fmt.Errorf("invalid Mach-O load commands")
	}
	commands := make([]byte, size)
	if _, err := m.r.ReadAt(commands, headerSize); err != nil {
		return 0, err
	}
	for range count {
		if len(commands) < 8 {
			return 0, fmt.Errorf("truncated Mach-O load command")
		}
		kind, commandSize := order.Uint32(commands[:4]), order.Uint32(commands[4:8])
		if commandSize < 8 || uint64(commandSize) > uint64(len(commands)) {
			return 0, fmt.Errorf("invalid Mach-O load command size")
		}
		if err := visit(order, kind, commands[:commandSize]); err != nil {
			return 0, err
		}
		commands = commands[commandSize:]
	}
	if len(commands) != 0 {
		return 0, fmt.Errorf("Mach-O command count and size disagree")
	}
	return headerSize + int64(size), nil
}

// embeddedInfoPlist returns the __TEXT,__info_plist section that standalone
// executables carry in place of a bundle Info.plist, sealed by special slot 1.
func (m machoSlice) embeddedInfoPlist() ([]byte, error) {
	var offset, size uint64
	_, err := m.loadCommands(func(order binary.ByteOrder, kind uint32, command []byte) error {
		headerSize, sectionSize, wide := 56, 68, false
		switch kind {
		case 0x1:
		case 0x19:
			headerSize, sectionSize, wide = 72, 80, true
		default:
			return nil
		}
		if len(command) < headerSize || string(bytes.TrimRight(command[8:24], "\x00")) != "__TEXT" {
			return nil
		}
		sections := command[headerSize:]
		for len(sections) >= sectionSize {
			section := sections[:sectionSize]
			sections = sections[sectionSize:]
			if string(bytes.TrimRight(section[:16], "\x00")) != "__info_plist" {
				continue
			}
			if size != 0 {
				return fmt.Errorf("duplicate __info_plist section")
			}
			if wide {
				size, offset = order.Uint64(section[40:48]), uint64(order.Uint32(section[48:52]))
			} else {
				size, offset = uint64(order.Uint32(section[36:40])), uint64(order.Uint32(section[40:44]))
			}
			if size == 0 || size > maxMetadata || offset > uint64(m.size) || size > uint64(m.size)-offset { //nolint:gosec // Slice sizes are non-negative.
				return fmt.Errorf("invalid __info_plist section")
			}
		}
		return nil
	})
	if err != nil || size == 0 {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := m.r.ReadAt(data, int64(offset)); err != nil {
		return nil, err
	}
	return data, nil
}

func (m machoSlice) signature() (*codeSignature, error) {
	var offset, length int64
	found := false
	end, err := m.loadCommands(func(order binary.ByteOrder, kind uint32, command []byte) error {
		if kind != 0x1d {
			return nil
		}
		if found || len(command) != 16 {
			return fmt.Errorf("invalid or duplicate LC_CODE_SIGNATURE")
		}
		found = true
		offset, length = int64(order.Uint32(command[8:12])), int64(order.Uint32(command[12:16]))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("Mach-O is unsigned")
	}
	if offset < end || length < 12 || length > maxSignature || offset > m.size || length != m.size-offset {
		return nil, fmt.Errorf("%w: Mach-O signature must be a bounded final region", ErrUnsupported)
	}
	data := make([]byte, length)
	if _, err := m.r.ReadAt(data, offset); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(data[:4]) != 0xfade0cc0 {
		return nil, fmt.Errorf("%w: signature is not an embedded SuperBlob", ErrUnsupported)
	}
	blobSize, blobCount := binary.BigEndian.Uint32(data[4:8]), binary.BigEndian.Uint32(data[8:12])
	if blobCount == 0 || blobCount > 64 || uint64(blobSize) > uint64(len(data)) || blobSize < 12+8*blobCount {
		return nil, fmt.Errorf("invalid signature SuperBlob size")
	}
	// LC_CODE_SIGNATURE bounds an allocation; the SuperBlob length bounds the
	// signature. Unused allocation bytes still contribute to artifact identity.
	data = data[:blobSize]
	sig := &codeSignature{codeOffset: offset, blobs: make(map[uint32][]byte)}
	type span struct{ start, end uint32 }
	var spans []span
	for i := range blobCount {
		index := data[12+8*i : 20+8*i]
		slot, start := binary.BigEndian.Uint32(index[:4]), binary.BigEndian.Uint32(index[4:8])
		if _, exists := sig.blobs[slot]; exists {
			return nil, fmt.Errorf("duplicate signature slot %d", slot)
		}
		if start < 12+8*blobCount || uint64(start)+8 > uint64(len(data)) {
			return nil, fmt.Errorf("invalid signature blob offset")
		}
		size := binary.BigEndian.Uint32(data[start+4 : start+8])
		if size < 8 || uint64(start)+uint64(size) > uint64(len(data)) {
			return nil, fmt.Errorf("invalid signature blob length")
		}
		blob := data[start : start+size]
		sig.blobs[slot] = blob
		spans = append(spans, span{start, start + size})
		if slot == 0 || slot >= 0x1000 && slot <= 0x1005 {
			if binary.BigEndian.Uint32(blob[:4]) != 0xfade0c02 {
				return nil, fmt.Errorf("invalid CodeDirectory magic")
			}
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return nil, fmt.Errorf("overlapping signature blobs")
		}
	}
	if sig.blobs[0] == nil {
		return nil, fmt.Errorf("missing primary CodeDirectory")
	}
	sig.directories = append(sig.directories, sig.blobs[0])
	for slot := uint32(0x1000); slot <= 0x1005; slot++ {
		if sig.blobs[slot] != nil {
			sig.directories = append(sig.directories, sig.blobs[slot])
		}
	}
	return sig, nil
}

func validateCodeDirectory(cd []byte) error {
	if len(cd) < 44 {
		return fmt.Errorf("truncated CodeDirectory")
	}
	version := binary.BigEndian.Uint32(cd[8:12])
	if version < 0x20001 || version > 0x20500 {
		return fmt.Errorf("%w: CodeDirectory version %#x", ErrUnsupported, version)
	}
	minimum := 44
	for _, v := range []struct {
		version uint32
		length  int
	}{{0x20100, 48}, {0x20200, 52}, {0x20300, 64}, {0x20400, 88}, {0x20500, 96}} {
		if version >= v.version {
			minimum = v.length
		}
	}
	if len(cd) < minimum {
		return fmt.Errorf("truncated versioned CodeDirectory header")
	}
	if version >= 0x20100 && binary.BigEndian.Uint32(cd[44:48]) != 0 {
		return fmt.Errorf("%w: scattered CodeDirectory", ErrUnsupported)
	}
	if version >= 0x20500 && binary.BigEndian.Uint32(cd[92:96]) != 0 {
		return fmt.Errorf("%w: pre-encrypted CodeDirectory", ErrUnsupported)
	}
	hashOffset := uint64(binary.BigEndian.Uint32(cd[16:20]))
	special := uint64(binary.BigEndian.Uint32(cd[24:28]))
	code := uint64(binary.BigEndian.Uint32(cd[28:32]))
	size := uint64(cd[36])
	if size == 0 || special > 64 || code > 1<<24 || hashOffset < uint64(minimum)+special*size || hashOffset+code*size > uint64(len(cd)) {
		return fmt.Errorf("invalid CodeDirectory hash table")
	}
	if _, err := codeString(cd, binary.BigEndian.Uint32(cd[20:24])); err != nil {
		return err
	}
	return nil
}

func codeString(cd []byte, offset uint32) (string, error) {
	end := uint64(binary.BigEndian.Uint32(cd[16:20])) - uint64(binary.BigEndian.Uint32(cd[24:28]))*uint64(cd[36])
	if offset < 44 || uint64(offset) >= end || end > uint64(len(cd)) {
		return "", fmt.Errorf("invalid CodeDirectory string offset")
	}
	text := cd[offset:end]
	value, _, found := bytes.Cut(text, []byte{0})
	if !found {
		return "", fmt.Errorf("unterminated CodeDirectory string")
	}
	return string(value), nil
}

func (m machoSlice) verifyCodeDirectory(cd []byte, sig *codeSignature, external map[uint32][]byte) error {
	if err := validateCodeDirectory(cd); err != nil {
		return err
	}
	h, size, err := codeHash(cd[37])
	if err != nil {
		return err
	}
	if int(cd[36]) != size {
		return fmt.Errorf("CodeDirectory hash size disagrees with algorithm")
	}
	limit := uint64(binary.BigEndian.Uint32(cd[32:36]))
	if binary.BigEndian.Uint32(cd[8:12]) >= 0x20300 && limit == 0 {
		limit = binary.BigEndian.Uint64(cd[56:64])
	}
	if limit != uint64(sig.codeOffset) { //nolint:gosec // codeOffset holds the uint32 LC_CODE_SIGNATURE offset.
		return fmt.Errorf("%w: CodeDirectory does not seal all bytes before signature", ErrUnsupported)
	}
	code := io.NewSectionReader(m.r, 0, sig.codeOffset)
	pageSize := code.Size()
	if cd[39] != 0 {
		if cd[39] > 30 {
			return fmt.Errorf("%w: CodeDirectory page size", ErrUnsupported)
		}
		pageSize = 1 << cd[39]
	}
	if pageSize == 0 {
		return fmt.Errorf("empty CodeDirectory code region")
	}
	codeSlots := int64(binary.BigEndian.Uint32(cd[28:32]))
	if codeSlots != (code.Size()+pageSize-1)/pageSize {
		return fmt.Errorf("CodeDirectory page count disagrees with code limit")
	}
	hashOffset := int64(binary.BigEndian.Uint32(cd[16:20]))
	buffer := make([]byte, min(pageSize, 32<<10))
	digest := make([]byte, 0, h.Size())
	for slot := range codeSlots {
		h.Reset()
		if _, err := io.CopyBuffer(h, io.LimitReader(code, pageSize), buffer); err != nil {
			return err
		}
		start := hashOffset + slot*int64(size)
		if !bytes.Equal(h.Sum(digest[:0])[:size], cd[start:start+int64(size)]) {
			return fmt.Errorf("code page %d hash mismatch", slot)
		}
	}
	for slot := uint32(1); slot <= binary.BigEndian.Uint32(cd[24:28]); slot++ {
		start := hashOffset - int64(slot)*int64(size)
		expected := cd[start : start+int64(size)]
		if allZero(expected) {
			if len(external[slot]) != 0 {
				return fmt.Errorf("CodeDirectory does not seal required external slot %d", slot)
			}
			continue
		}
		value := sig.blobs[slot]
		if slot == 1 || slot == 3 {
			value = external[slot]
		}
		if len(value) == 0 {
			return fmt.Errorf("%w: unavailable CodeDirectory special slot %d", ErrUnsupported, slot)
		}
		h.Reset()
		h.Write(value)
		if !bytes.Equal(h.Sum(digest[:0])[:size], expected) {
			return fmt.Errorf("special slot %d hash mismatch", slot)
		}
	}
	for slot := range external {
		if slot > binary.BigEndian.Uint32(cd[24:28]) {
			return fmt.Errorf("CodeDirectory lacks required external slot %d", slot)
		}
	}
	return nil
}

func codeHash(kind byte) (hash.Hash, int, error) {
	switch kind {
	case 1:
		return sha1.New(), 20, nil //nolint:gosec // CodeDirectory hash type 1 is SHA-1.
	case 2:
		return sha256.New(), 32, nil
	case 3:
		return sha256.New(), 20, nil
	case 4:
		return sha512.New384(), 48, nil
	default:
		return nil, 0, fmt.Errorf("%w: CodeDirectory hash type %d", ErrUnsupported, kind)
	}
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}
