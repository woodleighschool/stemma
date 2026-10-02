package apple

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/woodleighschool/stemma/internal/signature"
)

// Nested code that is not a Mach-O, such as a script or data file in a code
// directory, is generic code. codesign keeps its signature in extended
// attributes named after the signature slots, and its CodeDirectories hash the
// whole file.

// xattrFS reports extended attributes, as a disk image does.
type xattrFS interface {
	XattrValues(name string) (map[string]appledouble.Value, error)
}

const signatureAttribute = "com.apple.cs."

// signatureSlots maps codesign's slot names to signature slots. Alternate
// CodeDirectories reuse the requirements name with a numeric suffix.
var signatureSlots = map[string]uint32{
	"CodeDirectory":      0,
	"CodeRequirements":   2,
	"CodeResources":      3,
	"CodeTopDirectory":   4,
	"CodeEntitlements":   5,
	"CodeRepSpecific":    6,
	"CodeEntitlementDER": 7,
	"CodeRequirements-1": 0x1000,
	"CodeRequirements-2": 0x1001,
	"CodeRequirements-3": 0x1002,
	"CodeRequirements-4": 0x1003,
	"CodeRequirements-5": 0x1004,
	"CodeSignature":      0x10000,
}

// isMachO reports whether code begins with a thin or universal Mach-O magic.
func isMachO(r io.ReaderAt, size int64) (bool, error) {
	if size < 4 {
		return false, nil
	}
	var magic [4]byte
	if _, err := r.ReadAt(magic[:], 0); err != nil {
		return false, err
	}
	switch binary.BigEndian.Uint32(magic[:]) {
	case 0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe, 0xcafebabe, 0xcafebabf:
		return true, nil
	}
	return false, nil
}

// verifyGeneric authenticates generic code at a location in the verified
// bundle from the signature in its extended attributes.
func (v *bundleVerifier) verifyGeneric(location string, r io.ReaderAt, size int64) (codeIdentity, error) {
	if v.attributes == nil {
		return codeIdentity{}, fmt.Errorf("%w: generic code requires extended attributes from its source", ErrUnsupported)
	}
	xattrs, err := v.attributes.XattrValues(path.Join(v.base, location))
	if err != nil {
		return codeIdentity{}, err
	}
	sig := &codeSignature{codeSize: size, blobs: map[uint32][]byte{}}
	for name, value := range xattrs {
		slotName, ok := strings.CutPrefix(name, signatureAttribute)
		if !ok {
			continue
		}
		slot, ok := signatureSlots[slotName]
		if !ok {
			return codeIdentity{}, fmt.Errorf("%w: signature attribute %s", ErrUnsupported, name)
		}
		if value == nil || value.Size() < 0 || value.Size() > maxSignature {
			return codeIdentity{}, fmt.Errorf("signature attribute %s exceeds its size limit", name)
		}
		blob := make([]byte, int(value.Size()))
		if _, err := io.ReadFull(io.NewSectionReader(contextReaderAt{v.ctx, value}, 0, value.Size()), blob); err != nil {
			return codeIdentity{}, fmt.Errorf("signature attribute %s: %w", name, err)
		}
		if slot == 0x10000 {
			// The CMS signature is stored bare; an embedded signature wraps it.
			header := make([]byte, 8, len(blob)+8)
			binary.BigEndian.PutUint32(header, 0xfade0b01)
			binary.BigEndian.PutUint32(header[4:], uint32(len(blob)+8)) //nolint:gosec // The length is bounded by maxSignature.
			blob = append(header, blob...)
		}
		if len(blob) < 8 || binary.BigEndian.Uint32(blob[4:8]) != uint32(len(blob)) { //nolint:gosec // The length is bounded by maxSignature.
			return codeIdentity{}, fmt.Errorf("invalid signature attribute %s", name)
		}
		if (slot == 0 || slot >= 0x1000 && slot <= 0x1004) && binary.BigEndian.Uint32(blob[:4]) != 0xfade0c02 {
			return codeIdentity{}, fmt.Errorf("invalid CodeDirectory magic in %s", name)
		}
		sig.blobs[slot] = blob
	}
	if len(sig.blobs) == 0 {
		return codeIdentity{}, signature.ErrUnsigned
	}
	if sig.blobs[0] == nil {
		return codeIdentity{}, errors.New("generic code signature has no CodeDirectory")
	}

	sig.directories = append(sig.directories, sig.blobs[0])
	for slot := uint32(0x1000); slot <= 0x1004; slot++ {
		if sig.blobs[slot] != nil {
			sig.directories = append(sig.directories, sig.blobs[slot])
		}
	}
	return verifySignature(contextReaderAt{v.ctx, r}, sig, nil)
}
