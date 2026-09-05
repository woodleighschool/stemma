package apple

import (
	"bytes"
	"compress/bzip2"
	"compress/zlib"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strings"
)

const maxTOC = 8 << 20
const maxMetadata = 4 << 20
const maxEntrySize = 16 << 30

// Entry describes a XAR entry without materializing or executing it.
type Entry struct {
	Path           string `json:"path"`
	Type           string `json:"type"`
	Size           int64  `json:"size"`
	CompressedSize int64  `json:"compressed_size"`
	Encoding       string `json:"encoding,omitempty"`
}

// PackageInfo contains receipt metadata from a component's PackageInfo XML.
type PackageInfo struct {
	Path            string `json:"path"`
	Identifier      string `json:"identifier" xml:"identifier,attr"`
	Version         string `json:"version" xml:"version,attr"`
	InstallLocation string `json:"install_location" xml:"install-location,attr"`
}

// PackageFacts describes the installer container, not a verified inner application.
type PackageFacts struct {
	Format       string        `json:"format"`
	Entries      []Entry       `json:"entries"`
	Packages     []PackageInfo `json:"packages"`
	HasSignature bool          `json:"has_signature"`
}

type xarChecksum struct {
	Style  string `xml:"style,attr"`
	Value  string `xml:",chardata"`
	Offset int64  `xml:"offset"`
	Size   int64  `xml:"size"`
}

type xarData struct {
	Offset   int64 `xml:"offset"`
	Size     int64 `xml:"size"`
	Length   int64 `xml:"length"`
	Encoding struct {
		Style string `xml:"style,attr"`
	} `xml:"encoding"`
	Archived  xarChecksum `xml:"archived-checksum"`
	Extracted xarChecksum `xml:"extracted-checksum"`
}

type xarFile struct {
	Names []string  `xml:"name"`
	Type  string    `xml:"type"`
	Data  *xarData  `xml:"data"`
	EA    []xarData `xml:"ea"`
	Files []xarFile `xml:"file"`
}

type xarSignature struct {
	Style        string   `xml:"style,attr"`
	Offset       int64    `xml:"offset"`
	Size         int64    `xml:"size"`
	Certificates []string `xml:"KeyInfo>X509Data>X509Certificate"`
}

type xarArchive struct {
	r      io.ReaderAt
	size   int64
	heap   int64
	digest []byte
	hash   crypto.Hash
	toc    struct {
		Checksum   xarChecksum    `xml:"checksum"`
		Signatures []xarSignature `xml:"signature"`
		Files      []xarFile      `xml:"file"`
	}
	entries []Entry
	files   map[string]xarFile
}

// InspectPackage reads a flat PKG/XAR table of contents and component receipt
// metadata. It preserves the installer and does not expand Payload or Scripts.
func InspectPackage(filePath string) (PackageFacts, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return PackageFacts{}, err
	}
	defer func() { _ = f.Close() }()
	archive, err := openXARFile(f)
	if err != nil {
		return PackageFacts{}, err
	}
	facts := PackageFacts{Format: "xar", Entries: archive.entries, HasSignature: len(archive.toc.Signatures) > 0}
	for _, entry := range archive.entries {
		if path.Base(entry.Path) != "PackageInfo" || entry.Type != "file" {
			continue
		}
		var data bytes.Buffer
		if err := archive.readEntry(entry.Path, &data, maxMetadata); err != nil {
			return facts, fmt.Errorf("%s: %w", entry.Path, err)
		}
		var metadata PackageInfo
		if err := xml.Unmarshal(data.Bytes(), &metadata); err != nil {
			return facts, fmt.Errorf("%s: %w", entry.Path, err)
		}
		metadata.Path = entry.Path
		facts.Packages = append(facts.Packages, metadata)
	}
	return facts, nil
}

// VerifyPackage checks all supported XAR data checksums and an RSA TOC signature
// when requested. Identity is limited to an exact signer certificate pin.
func VerifyPackage(filePath string, policy Policy) (Evidence, error) {
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
	archive, parseErr := openXARFile(f)
	if policy.RequireResources {
		evidence.Resources = Check{Status: Unsupported, Detail: "PKG container verification does not verify inner application resource seals"}
	}
	if policy.RequirePlatform {
		evidence.Platform = Check{Status: Unsupported, Detail: "macOS platform assessment requires native OS policy"}
	}
	if parseErr != nil {
		if policy.RequireIntegrity {
			evidence.Integrity = checkError(parseErr, "")
		}
		if policy.RequireSignature {
			evidence.Signature = checkError(parseErr, "")
		}
		if policy.RequireIdentity || policy.CertificateSHA256 != "" {
			evidence.Identity = checkError(parseErr, "")
		}
		return evidence, parseErr
	}
	if policy.RequireIntegrity {
		var integrityErr error
		for _, entry := range archive.entries {
			file := archive.files[entry.Path]
			for i, ea := range file.EA {
				if integrityErr = archive.readData(fmt.Sprintf("%s extended attribute %d", entry.Path, i), ea, io.Discard, maxEntrySize); integrityErr != nil {
					break
				}
			}
			if integrityErr != nil {
				break
			}
			if entry.Type != "file" && entry.Type != "directory" && entry.Type != "symlink" {
				integrityErr = fmt.Errorf("%w: XAR entry type %q", ErrUnsupported, entry.Type)
				break
			}
			if entry.Type != "file" {
				continue
			}
			if integrityErr = archive.readEntry(entry.Path, io.Discard, maxEntrySize); integrityErr != nil {
				break
			}
		}
		evidence.Integrity = checkError(integrityErr, "compressed TOC and every regular-file archived/extracted checksum match")
	}
	if policy.RequireSignature || policy.RequireIdentity || policy.CertificateSHA256 != "" {
		cert, signatureErr := archive.verifySignature()
		if policy.RequireSignature {
			evidence.Signature = checkError(signatureErr, "RSA PKCS#1 v1.5 signature binds the TOC digest; no chain or timestamp assessment")
		}
		if policy.RequireIdentity || policy.CertificateSHA256 != "" {
			identityErr := signatureErr
			if identityErr == nil {
				pin, pinErr := hex.DecodeString(policy.CertificateSHA256)
				switch {
				case policy.CertificateSHA256 == "":
					identityErr = fmt.Errorf("%w: identity requires an explicit certificate SHA-256 pin", ErrUnsupported)
				case pinErr != nil || len(pin) != sha256.Size:
					identityErr = fmt.Errorf("certificate pin must be 64 hexadecimal characters")
				default:
					actual := sha256.Sum256(cert.Raw)
					if subtle.ConstantTimeCompare(pin, actual[:]) != 1 {
						identityErr = fmt.Errorf("signer certificate SHA-256 does not match configured pin")
					}
				}
			}
			evidence.Identity = checkError(identityErr, "signing certificate matches exact configured SHA-256 pin; expiry, roots, revocation and timestamps not assessed")
		}
	}
	return evidence, evidence.required(policy)
}

func openXARFile(f *os.File) (*xarArchive, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("XAR input must be a regular file")
	}
	return openXAR(f, info.Size())
}

func openXAR(r io.ReaderAt, size int64) (*xarArchive, error) {
	var header [28]byte
	if _, err := r.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	if string(header[:4]) != "xar!" {
		return nil, fmt.Errorf("not a XAR archive")
	}
	if binary.BigEndian.Uint16(header[6:8]) != 1 {
		return nil, fmt.Errorf("%w: XAR version", ErrUnsupported)
	}
	headerSize := int64(binary.BigEndian.Uint16(header[4:6]))
	compressed := binary.BigEndian.Uint64(header[8:16])
	uncompressed := binary.BigEndian.Uint64(header[16:24])
	if headerSize != 28 {
		return nil, fmt.Errorf("%w: extended XAR header", ErrUnsupported)
	}
	if compressed == 0 || compressed > maxTOC || uncompressed == 0 || uncompressed > maxTOC || int64(compressed) > size-headerSize {
		return nil, fmt.Errorf("invalid or oversized XAR table of contents")
	}
	compression := make([]byte, compressed)
	if _, err := r.ReadAt(compression, headerSize); err != nil {
		return nil, err
	}
	compressedReader := bytes.NewReader(compression)
	zr, err := zlib.NewReader(compressedReader)
	if err != nil {
		return nil, fmt.Errorf("XAR TOC: %w", err)
	}
	defer func() { _ = zr.Close() }()
	toc, err := io.ReadAll(io.LimitReader(zr, int64(uncompressed)+1))
	if err != nil {
		return nil, fmt.Errorf("XAR TOC: %w", err)
	}
	if len(toc) != int(uncompressed) || compressedReader.Len() != 0 {
		return nil, fmt.Errorf("XAR TOC lengths disagree")
	}
	if err := validateTOCXML(toc); err != nil {
		return nil, err
	}
	archive := &xarArchive{r: r, size: size, heap: headerSize + int64(compressed), files: make(map[string]xarFile)}
	var root struct {
		XMLName xml.Name `xml:"xar"`
		TOCs    []struct {
			Checksum   xarChecksum    `xml:"checksum"`
			Signatures []xarSignature `xml:"signature"`
			Files      []xarFile      `xml:"file"`
		} `xml:"toc"`
	}
	if err := xml.Unmarshal(toc, &root); err != nil {
		return nil, fmt.Errorf("XAR TOC XML: %w", err)
	}
	if len(root.TOCs) != 1 {
		return nil, fmt.Errorf("XAR must contain one TOC")
	}
	archive.toc = root.TOCs[0]
	algorithm := binary.BigEndian.Uint32(header[24:28])
	styles := map[uint32]string{1: "sha1", 3: "sha256", 4: "sha512"}
	style, ok := styles[algorithm]
	if !ok {
		return nil, fmt.Errorf("%w: XAR TOC checksum algorithm %d", ErrUnsupported, algorithm)
	}
	if strings.ToLower(archive.toc.Checksum.Style) != style {
		return nil, fmt.Errorf("XAR header and TOC checksum algorithms disagree")
	}
	h, hashID, _ := xarHash(style)
	h.Write(compression)
	archive.digest, archive.hash = h.Sum(nil), hashID
	checksum := archive.toc.Checksum
	if checksum.Size != int64(h.Size()) {
		return nil, fmt.Errorf("invalid XAR TOC checksum length")
	}
	stored, err := archive.heapBytes(checksum.Offset, checksum.Size, 64)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(stored, archive.digest) {
		return nil, fmt.Errorf("XAR TOC checksum mismatch")
	}
	if err := archive.addFiles("", archive.toc.Files, 0); err != nil {
		return nil, err
	}
	return archive, nil
}

func validateTOCXML(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var parents []map[string]bool
	roots, nodes := 0, 0
	singletons := map[string]bool{"toc": true, "checksum": true, "type": true, "data": true, "offset": true, "size": true, "length": true, "encoding": true, "archived-checksum": true, "extracted-checksum": true, "KeyInfo": true, "X509Data": true}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("XAR TOC XML: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			nodes++
			if nodes > 250000 || len(parents) >= 96 {
				return fmt.Errorf("XAR TOC XML exceeds structural limits")
			}
			if len(parents) == 0 {
				roots++
			} else if singletons[token.Name.Local] {
				parent := parents[len(parents)-1]
				if parent[token.Name.Local] {
					return fmt.Errorf("duplicate XAR element %q", token.Name.Local)
				}
				parent[token.Name.Local] = true
			}
			attributes := make(map[string]bool)
			for _, attr := range token.Attr {
				if attributes[attr.Name.Local] {
					return fmt.Errorf("duplicate XAR attribute %q", attr.Name.Local)
				}
				attributes[attr.Name.Local] = true
			}
			parents = append(parents, make(map[string]bool))
		case xml.EndElement:
			parents = parents[:len(parents)-1]
		case xml.CharData:
			if len(parents) == 0 && len(bytes.TrimSpace(token)) != 0 {
				return fmt.Errorf("trailing XAR XML content")
			}
		}
	}
	if roots != 1 {
		return fmt.Errorf("XAR TOC requires exactly one XML root")
	}
	return nil
}

func (a *xarArchive) addFiles(parent string, files []xarFile, depth int) error {
	if depth > 64 {
		return fmt.Errorf("XAR nesting exceeds 64 levels")
	}
	for _, file := range files {
		if len(file.Names) == 0 {
			return fmt.Errorf("XAR entry has no filename")
		}
		baseName := file.Names[0]
		for _, other := range file.Names[1:] {
			if other != baseName {
				return fmt.Errorf("contradictory XAR filenames %q and %q", baseName, other)
			}
		}
		if baseName == "" || baseName == "." || baseName == ".." || strings.ContainsAny(baseName, "/\\\x00:") {
			return fmt.Errorf("unsafe XAR filename %q", baseName)
		}
		name := path.Join(parent, baseName)
		if _, exists := a.files[name]; exists {
			return fmt.Errorf("duplicate XAR path %q", name)
		}
		if len(a.files) >= 100000 {
			return fmt.Errorf("too many XAR entries")
		}
		entry := Entry{Path: name, Type: file.Type}
		if file.Data != nil {
			data := file.Data
			if data.Size < 0 || data.Length < 0 || !a.validHeapRange(data.Offset, data.Length) {
				return fmt.Errorf("invalid XAR data range for %s", name)
			}
			entry.Size, entry.CompressedSize, entry.Encoding = data.Size, data.Length, data.Encoding.Style
		}
		a.files[name] = file
		a.entries = append(a.entries, entry)
		if len(file.Files) != 0 && file.Type != "directory" {
			return fmt.Errorf("XAR children in non-directory %q", name)
		}
		if err := a.addFiles(name, file.Files, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (a *xarArchive) validHeapRange(offset, size int64) bool {
	return offset >= 0 && size >= 0 && offset <= a.size-a.heap && size <= a.size-a.heap-offset
}

func (a *xarArchive) heapBytes(offset, size, limit int64) ([]byte, error) {
	if !a.validHeapRange(offset, size) || size > limit {
		return nil, fmt.Errorf("invalid or oversized XAR heap range")
	}
	data := make([]byte, size)
	_, err := a.r.ReadAt(data, a.heap+offset)
	return data, err
}

func (a *xarArchive) readEntry(name string, dst io.Writer, limit int64) error {
	file, ok := a.files[name]
	if !ok || file.Type != "file" {
		return fmt.Errorf("XAR entry %q is not a regular file", name)
	}
	if file.Data == nil {
		return fmt.Errorf("%w: XAR regular file without data descriptor", ErrUnsupported)
	}
	return a.readData(name, *file.Data, dst, limit)
}

func (a *xarArchive) readData(name string, data xarData, dst io.Writer, limit int64) error {
	if data.Size < 0 || data.Length < 0 || !a.validHeapRange(data.Offset, data.Length) {
		return fmt.Errorf("invalid XAR data range for %s", name)
	}
	if data.Size > limit || data.Length > maxEntrySize {
		return fmt.Errorf("XAR entry %q exceeds read limit", name)
	}
	archived, _, err := xarHash(data.Archived.Style)
	if err != nil {
		return err
	}
	extracted, _, err := xarHash(data.Extracted.Style)
	if err != nil {
		return err
	}
	section := io.NewSectionReader(a.r, a.heap+data.Offset, data.Length)
	compressed := io.TeeReader(section, archived)
	source := compressed
	switch data.Encoding.Style {
	case "application/octet-stream":
	case "application/x-gzip":
		zr, err := zlib.NewReader(compressed)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		source = zr
	case "application/x-bzip2":
		source = bzip2.NewReader(compressed)
	default:
		return fmt.Errorf("%w: XAR compression %q", ErrUnsupported, data.Encoding.Style)
	}
	n, err := io.Copy(io.MultiWriter(dst, extracted), io.LimitReader(source, data.Size+1))
	if err != nil {
		return fmt.Errorf("XAR entry %s: %w", name, err)
	}
	if n != data.Size {
		return fmt.Errorf("XAR entry %s extracted size mismatch", name)
	}
	// Include any compressed-stream padding in the archive checksum as XAR does.
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return err
	}
	for _, sum := range []struct {
		actual   []byte
		expected string
	}{{archived.Sum(nil), data.Archived.Value}, {extracted.Sum(nil), data.Extracted.Value}} {
		expected, err := hex.DecodeString(strings.TrimSpace(sum.expected))
		if err != nil || !bytes.Equal(expected, sum.actual) {
			return fmt.Errorf("XAR entry %s checksum mismatch", name)
		}
	}
	return nil
}

func (a *xarArchive) verifySignature() (*x509.Certificate, error) {
	if len(a.toc.Signatures) == 0 {
		return nil, fmt.Errorf("XAR archive is unsigned")
	}
	if len(a.toc.Signatures) != 1 {
		return nil, fmt.Errorf("%w: multiple XAR signatures", ErrUnsupported)
	}
	sig := a.toc.Signatures[0]
	if sig.Style != "RSA" {
		return nil, fmt.Errorf("%w: XAR signature style %q", ErrUnsupported, sig.Style)
	}
	if len(sig.Certificates) == 0 {
		return nil, fmt.Errorf("XAR signature has no signer certificate")
	}
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(sig.Certificates[0]), ""))
	if err != nil {
		return nil, fmt.Errorf("XAR signer certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("XAR signer certificate: %w", err)
	}
	key, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: XAR signer public-key type", ErrUnsupported)
	}
	if key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("%w: RSA keys below 2048 bits", ErrUnsupported)
	}
	signature, err := a.heapBytes(sig.Offset, sig.Size, 16384)
	if err != nil {
		return nil, err
	}
	if err := rsa.VerifyPKCS1v15(key, a.hash, a.digest, signature); err != nil {
		return nil, fmt.Errorf("XAR RSA signature invalid: %w", err)
	}
	return cert, nil
}

func xarHash(style string) (hash.Hash, crypto.Hash, error) {
	switch strings.ToLower(style) {
	case "sha1":
		return sha1.New(), crypto.SHA1, nil
	case "sha256":
		return sha256.New(), crypto.SHA256, nil
	case "sha512":
		return sha512.New(), crypto.SHA512, nil
	default:
		return nil, 0, fmt.Errorf("%w: XAR checksum %q", ErrUnsupported, style)
	}
}
