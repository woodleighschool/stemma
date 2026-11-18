package apple

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/treefs"
)

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

// A generic file can carry its own signature even inside a signed bundle.
func (v *bundleVerifier) hasSignatureAttributes(location string, file *treefs.File) (bool, error) {
	if err := v.ctx.Err(); err != nil {
		return false, err
	}
	if v.attributes != nil {
		attributes, err := v.attributes.XattrValues(path.Join(v.base, location))
		if err != nil {
			return false, err
		}
		for name := range attributes {
			if strings.HasPrefix(name, signatureAttribute) {
				return true, nil
			}
		}
		return false, v.ctx.Err()
	}
	if local, ok := file.File.(*os.File); ok {
		signed, err := nativeSignatureAttributes(v.ctx, local)
		if err != nil {
			return false, fmt.Errorf("read signature attributes: %w", err)
		}
		return signed, v.ctx.Err()
	}
	return false, fmt.Errorf("%w: generic executable requires extended attributes from its source", ErrUnsupported)
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
	blobs := map[uint32][]byte{}
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
		blobs[slot] = blob
	}
	if len(blobs) == 0 {
		return codeIdentity{}, signature.ErrUnsigned
	}
	return v.verifyComponents(r, size, blobs, nil)
}

// Both extended attributes and bundle files store CMS bare, while every other
// signature component carries its blob header.
func (v *bundleVerifier) verifyComponents(code io.ReaderAt, size int64, blobs, external map[uint32][]byte) (codeIdentity, error) {
	for slot, blob := range blobs {
		if slot == 0x10000 {
			wrapped := make([]byte, 8, 8+len(blob))
			binary.BigEndian.PutUint32(wrapped, 0xfade0b01)
			binary.BigEndian.PutUint32(wrapped[4:], uint32(len(blob)+8)) //nolint:gosec // Both readers bound signature size.
			blobs[slot] = append(wrapped, blob...)
			continue
		}
		if len(blob) < 8 || binary.BigEndian.Uint32(blob[4:8]) != uint32(len(blob)) { //nolint:gosec // Both readers bound signature size.
			return codeIdentity{}, fmt.Errorf("invalid signature blob in slot %d", slot)
		}
	}
	sig, err := signatureFromBlobs(blobs, size)
	if err != nil {
		return codeIdentity{}, err
	}
	return verifySignature(contextReaderAt{v.ctx, code}, sig, external, v.roots)
}
