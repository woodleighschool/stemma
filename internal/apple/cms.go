package apple

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"fmt"

	"github.com/smallstep/pkcs7"
	"howett.net/plist"
)

var oidHashAgility = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 9, 1}
var oidHashAgilityV2 = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 9, 2}

func verifyCMS(signature *codeSignature) (*x509.Certificate, error) {
	if len(signature.directories) != 1 {
		return nil, fmt.Errorf("%w: CMS authentication of alternate CodeDirectories", ErrUnsupported)
	}
	cd := signature.directories[0]
	if err := validateCodeDirectory(cd); err != nil {
		return nil, err
	}
	if cd[37] != 2 {
		return nil, fmt.Errorf("%w: CMS authentication requires a SHA-256 CodeDirectory", ErrUnsupported)
	}
	blob := signature.blobs[0x10000]
	if len(blob) <= 8 {
		return nil, fmt.Errorf("Mach-O has no CMS signature; ad-hoc hashes do not authenticate a signer")
	}
	if binary.BigEndian.Uint32(blob[:4]) != 0xfade0b01 {
		return nil, fmt.Errorf("invalid CMS signature blob")
	}
	if err := validateCMSEncoding(blob[8:]); err != nil {
		return nil, err
	}
	signed, err := pkcs7.Parse(blob[8:])
	if err != nil {
		return nil, fmt.Errorf("CMS parse: %w", err)
	}
	if len(signed.Content) != 0 || len(signed.Signers) != 1 {
		return nil, fmt.Errorf("%w: CMS requires one signer and detached CodeDirectory content", ErrUnsupported)
	}
	signer := signed.Signers[0]
	if !signer.DigestAlgorithm.Algorithm.Equal(pkcs7.OIDDigestAlgorithmSHA256) || (!signer.DigestEncryptionAlgorithm.Algorithm.Equal(pkcs7.OIDEncryptionAlgorithmRSA) && !signer.DigestEncryptionAlgorithm.Algorithm.Equal(pkcs7.OIDEncryptionAlgorithmRSASHA256)) {
		return nil, fmt.Errorf("%w: CMS requires RSA PKCS1v15 with SHA-256", ErrUnsupported)
	}
	for _, parameters := range []asn1.RawValue{signer.DigestAlgorithm.Parameters, signer.DigestEncryptionAlgorithm.Parameters} {
		if len(parameters.FullBytes) != 0 && !bytes.Equal(parameters.FullBytes, []byte{5, 0}) {
			return nil, fmt.Errorf("%w: CMS algorithm parameters", ErrUnsupported)
		}
	}
	certificate := signed.GetOnlySigner()
	if certificate == nil {
		return nil, fmt.Errorf("CMS signer certificate is missing or ambiguous")
	}
	key, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 2048 || key.N.BitLen() > 8192 {
		return nil, fmt.Errorf("%w: CMS signer requires a 2048-8192 bit RSA key", ErrUnsupported)
	}
	digest := sha256.Sum256(cd)
	seen := map[string]bool{}
	for _, attribute := range signer.AuthenticatedAttributes {
		name := attribute.Type.String()
		if seen[name] || attribute.Value.Class != 0 || attribute.Value.Tag != asn1.TagSet || !attribute.Value.IsCompound {
			return nil, fmt.Errorf("invalid or duplicate CMS signed attribute %s", name)
		}
		seen[name] = true
		switch {
		case attribute.Type.Equal(pkcs7.OIDAttributeContentType):
			var contentType asn1.ObjectIdentifier
			if err := decodeCMSAttribute(attribute.Value.Bytes, &contentType); err != nil || !contentType.Equal(pkcs7.OIDData) {
				return nil, fmt.Errorf("CMS signed content type must be data")
			}
		case attribute.Type.Equal(pkcs7.OIDAttributeMessageDigest):
			var value []byte
			if err := decodeCMSAttribute(attribute.Value.Bytes, &value); err != nil {
				return nil, err
			}
		case attribute.Type.Equal(oidHashAgility):
			var data []byte
			if err := decodeCMSAttribute(attribute.Value.Bytes, &data); err != nil {
				return nil, err
			}
			var agility struct {
				Hashes [][]byte `plist:"cdhashes"`
			}
			if _, err := plist.Unmarshal(data, &agility); err != nil || len(agility.Hashes) != 1 || !bytes.Equal(agility.Hashes[0], digest[:20]) {
				return nil, fmt.Errorf("CMS hash agility does not match the primary CodeDirectory")
			}
		case attribute.Type.Equal(oidHashAgilityV2):
			var agility struct {
				Algorithm asn1.ObjectIdentifier
				Digest    []byte
			}
			if err := decodeCMSAttribute(attribute.Value.Bytes, &agility); err != nil || !agility.Algorithm.Equal(pkcs7.OIDDigestAlgorithmSHA256) || !bytes.Equal(agility.Digest, digest[:]) {
				return nil, fmt.Errorf("CMS hash agility v2 does not match the primary CodeDirectory")
			}
		}
	}
	if !seen[pkcs7.OIDAttributeContentType.String()] || !seen[pkcs7.OIDAttributeMessageDigest.String()] {
		return nil, fmt.Errorf("CMS requires signed content-type and message-digest attributes")
	}
	signed.Content = cd
	// Verify authenticates the detached bytes with the embedded signer certificate.
	// No trust store is supplied; certificate chains and timestamps are not trusted.
	if err := signed.Verify(); err != nil {
		return nil, fmt.Errorf("CMS signature: %w", err)
	}
	return certificate, nil
}

// Native signatures use both DER and indefinite-length BER. The CMS library
// converts BER recursively and ignores trailing input, so bound its framing first.
func validateCMSEncoding(data []byte) error {
	if len(data) == 0 || len(data) > 1<<20 {
		return fmt.Errorf("CMS encoding exceeds size bounds")
	}
	count := 0
	var visit func([]byte, int) ([]byte, error)
	visit = func(input []byte, depth int) ([]byte, error) {
		count++
		if depth > 32 || count > 16384 {
			return nil, fmt.Errorf("CMS encoding exceeds structure bounds")
		}
		if len(input) < 2 || input[0] == 0 {
			return nil, fmt.Errorf("invalid CMS encoding element")
		}
		if input[1] == 0x80 {
			if input[0]&0x20 == 0 || input[0]&0x1f == 0x1f {
				return nil, fmt.Errorf("unsupported CMS indefinite encoding")
			}
			content := input[2:]
			for {
				if len(content) >= 2 && content[0] == 0 && content[1] == 0 {
					return content[2:], nil
				}
				var err error
				content, err = visit(content, depth+1)
				if err != nil {
					return nil, err
				}
			}
		}
		var value asn1.RawValue
		rest, err := asn1.Unmarshal(input, &value)
		if err != nil {
			return nil, fmt.Errorf("CMS encoding structure: %w", err)
		}
		if value.IsCompound {
			for content := value.Bytes; len(content) != 0; {
				content, err = visit(content, depth+1)
				if err != nil {
					return nil, err
				}
			}
		}
		return rest, nil
	}
	rest, err := visit(data, 0)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("CMS encoding has trailing data")
	}
	return nil
}

func decodeCMSAttribute(data []byte, value any) error {
	rest, err := asn1.Unmarshal(data, value)
	if err != nil {
		return fmt.Errorf("CMS signed attribute: %w", err)
	}
	if len(rest) != 0 {
		return fmt.Errorf("CMS signed attribute contains multiple values")
	}
	return nil
}
