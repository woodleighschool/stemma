package apple

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
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

// VerifyMachO verifies page and available special-slot hashes for every slice.
// It does not authenticate CMS, evaluate trust or assess macOS acceptance.
// Nonzero bundle slots require VerifyApp, which supplies their exact bytes.
func VerifyMachO(filePath string, policy Policy) (Evidence, error) {
	policy = policy.expanded()
	f, err := os.Open(filePath)
	if err != nil {
		return Evidence{}, err
	}
	defer func() { _ = f.Close() }()
	digest, err := fileDigest(f)
	if err != nil {
		return Evidence{}, err
	}
	evidence := newEvidence(digest, policy)
	if policy.RequireResources {
		evidence.Resources = Check{Status: Unsupported, Detail: "resource scope requires an app bundle subject"}
	}
	if err := verifyExecutable(f, policy, nil, &evidence); err != nil {
		return evidence, err
	}
	return evidence, evidence.required(policy)
}

func verifyExecutable(f *os.File, policy Policy, external map[uint32][]byte, evidence *Evidence) error {
	if policy.RequirePlatform {
		evidence.Platform = Check{Status: Unsupported, Detail: "macOS platform assessment requires native OS policy"}
	}
	if policy.RequireIdentity || policy.CertificateSHA256 != "" {
		evidence.Identity = Check{Status: Unsupported, Detail: "Mach-O CMS identity and trust verification are unsupported"}
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Mach-O subject must be a regular file")
	}
	slices, parseErr := machoSlices(f, info.Size())
	var integrityErr, signatureErr error
	if parseErr != nil {
		integrityErr, signatureErr = parseErr, parseErr
	}
	for _, slice := range slices {
		signature, err := slice.signature()
		if err != nil {
			integrityErr, signatureErr = err, err
			break
		}
		if len(signature.blobs[0x10000]) > 8 {
			signatureErr = fmt.Errorf("%w: Mach-O CMS cryptographic signature verification", ErrUnsupported)
		} else if signatureErr == nil {
			signatureErr = fmt.Errorf("Mach-O has no CMS signature; ad-hoc hashes do not authenticate a signer")
		}
		if policy.RequireIntegrity || policy.RequireResources {
			for _, directory := range signature.directories {
				if err := slice.verifyCodeDirectory(directory, signature, external); err != nil {
					integrityErr = err
					break
				}
			}
		}
		if integrityErr != nil {
			break
		}
	}
	if policy.RequireIntegrity || policy.RequireResources {
		evidence.Integrity = checkError(integrityErr, "all architecture CodeDirectory page and special-slot hashes match; recorded hashes are not authenticated")
	}
	if policy.RequireSignature {
		evidence.Signature = checkError(signatureErr, "")
	}
	return nil
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
	entrySize := int64(20)
	if magic == 0xcafebabf {
		entrySize = 32
	}
	tableEnd := 8 + entrySize*int64(count)
	if tableEnd > size {
		return nil, fmt.Errorf("truncated universal Mach-O table")
	}
	table := make([]byte, entrySize*int64(count))
	if _, err := r.ReadAt(table, 8); err != nil {
		return nil, err
	}
	var slices []machoSlice
	type span struct{ start, end uint64 }
	var spans []span
	for i := range count {
		entry := table[int64(i)*entrySize : int64(i+1)*entrySize]
		cpu := binary.BigEndian.Uint32(entry[:4])
		offset, length := uint64(binary.BigEndian.Uint32(entry[8:12])), uint64(binary.BigEndian.Uint32(entry[12:16]))
		align := binary.BigEndian.Uint32(entry[16:20])
		if entrySize == 32 {
			offset, length, align = binary.BigEndian.Uint64(entry[8:16]), binary.BigEndian.Uint64(entry[16:24]), binary.BigEndian.Uint32(entry[24:28])
		}
		if offset < uint64(tableEnd) || length < 28 || offset > uint64(size) || length > uint64(size)-offset || align > 31 || offset%(uint64(1)<<align) != 0 {
			return nil, fmt.Errorf("invalid universal Mach-O slice range")
		}
		section := io.NewSectionReader(r, int64(offset), int64(length))
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
		slices = append(slices, machoSlice{r: section, size: int64(length), cpu: cpu})
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

func (m machoSlice) signature() (*codeSignature, error) {
	var header [28]byte
	if _, err := m.r.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	order, headerSize, err := machoOrder(binary.BigEndian.Uint32(header[:4]))
	if err != nil {
		return nil, err
	}
	count, size := order.Uint32(header[16:20]), order.Uint32(header[20:24])
	if count > 65536 || size > 16<<20 || int64(size) > m.size-headerSize {
		return nil, fmt.Errorf("invalid Mach-O load commands")
	}
	commands := make([]byte, size)
	if _, err := m.r.ReadAt(commands, headerSize); err != nil {
		return nil, err
	}
	var offset, length int64
	found := false
	for range count {
		if len(commands) < 8 {
			return nil, fmt.Errorf("truncated Mach-O load command")
		}
		kind, commandSize := order.Uint32(commands[:4]), order.Uint32(commands[4:8])
		if commandSize < 8 || uint64(commandSize) > uint64(len(commands)) {
			return nil, fmt.Errorf("invalid Mach-O load command size")
		}
		if kind == 0x1d {
			if found || commandSize != 16 {
				return nil, fmt.Errorf("invalid or duplicate LC_CODE_SIGNATURE")
			}
			found = true
			offset, length = int64(order.Uint32(commands[8:12])), int64(order.Uint32(commands[12:16]))
		}
		commands = commands[commandSize:]
	}
	if len(commands) != 0 {
		return nil, fmt.Errorf("Mach-O command count and size disagree")
	}
	if !found {
		return nil, fmt.Errorf("Mach-O is unsigned")
	}
	if offset < headerSize+int64(size) || length < 12 || length > maxSignature || offset > m.size || length != m.size-offset {
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
	if !allZero(data[blobSize:]) {
		return nil, fmt.Errorf("nonzero bytes beyond signature SuperBlob")
	}
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
	if limit != uint64(sig.codeOffset) {
		return fmt.Errorf("%w: CodeDirectory does not seal all bytes before signature", ErrUnsupported)
	}
	pageSize := limit
	if cd[39] != 0 {
		if cd[39] > 30 {
			return fmt.Errorf("%w: CodeDirectory page size", ErrUnsupported)
		}
		pageSize = uint64(1) << cd[39]
	}
	if pageSize == 0 {
		return fmt.Errorf("empty CodeDirectory code region")
	}
	codeSlots := uint64(binary.BigEndian.Uint32(cd[28:32]))
	if codeSlots != (limit+pageSize-1)/pageSize {
		return fmt.Errorf("CodeDirectory page count disagrees with code limit")
	}
	hashOffset := int64(binary.BigEndian.Uint32(cd[16:20]))
	for slot := range codeSlots {
		h.Reset()
		offset := slot * pageSize
		if _, err := io.Copy(h, io.NewSectionReader(m.r, int64(offset), int64(min(pageSize, limit-offset)))); err != nil {
			return err
		}
		start := hashOffset + int64(slot)*int64(size)
		if !bytes.Equal(h.Sum(nil)[:size], cd[start:start+int64(size)]) {
			return fmt.Errorf("Mach-O CPU %#x code page %d hash mismatch", m.cpu, slot)
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
		if !bytes.Equal(h.Sum(nil)[:size], expected) {
			return fmt.Errorf("Mach-O special slot %d hash mismatch", slot)
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
		return sha1.New(), 20, nil
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
