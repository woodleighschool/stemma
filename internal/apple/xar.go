package apple

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/deploymenttheory/go-macos-pkg/pkg/pkgsign"
	"github.com/deploymenttheory/go-macos-pkg/pkg/xar"
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
// InstalledSize is the declared payload size in KiB.
type PackageInfo struct {
	Path            string `json:"path" xml:"-"`
	Identifier      string `json:"identifier" xml:"identifier,attr"`
	Version         string `json:"version" xml:"version,attr"`
	InstallLocation string `json:"install_location" xml:"install-location,attr"`
	InstalledSize   int64  `json:"installed_size,omitempty" xml:"-"`
	HasPayload      bool   `json:"has_payload" xml:"-"`
}

// PackageFacts describes the installer container, not a verified inner application.
type PackageFacts struct {
	Format       string        `json:"format"`
	Entries      []Entry       `json:"entries"`
	Packages     []PackageInfo `json:"packages"`
	HasSignature bool          `json:"has_signature"`
	Applications []PackageApp  `json:"applications,omitempty"`
}

type xarArchive struct {
	reader  *xar.Reader
	entries []Entry
	files   map[string]*xar.File
}

// InspectPackage reads a flat PKG/XAR table of contents and component receipt
// metadata. It preserves the installer and does not expand Payload or Scripts.
func InspectPackage(filePath string) (PackageFacts, error) {
	return InspectPackageMetadata(context.Background(), filePath)
}

// VerifyPackage checks XAR data checksums and package signatures
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
			for _, ea := range file.EAs {
				if integrityErr = archive.verifyEA(ea); integrityErr != nil {
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
			if file.Data == nil || !hasPackageChecksums(file.Data.ArchivedChecksum, file.Data.ExtractedChecksum) {
				integrityErr = fmt.Errorf("XAR entry %q requires archived and extracted checksums", entry.Path)
				break
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
			evidence.Signature = checkError(signatureErr, "XAR signatures verified by pkgsign; certificate chain trust is not required")
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
			evidence.Identity = checkError(identityErr, "signing certificate matches exact configured SHA-256 pin; platform trust is not asserted")
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
	header, err := xar.ReadHeader(r)
	if err != nil {
		return nil, err
	}
	if header.TOCCompressed > maxTOC || header.TOCUncompressed > maxTOC {
		return nil, fmt.Errorf("XAR table of contents exceeds read limit")
	}
	x, err := xar.Open(r, size)
	if err != nil {
		return nil, err
	}
	if !x.TOCDigestValid() {
		return nil, fmt.Errorf("XAR TOC checksum mismatch")
	}
	if err := validatePackageXAR(x); err != nil {
		return nil, err
	}
	a := &xarArchive{reader: x, files: make(map[string]*xar.File)}
	duplicateBytes := int64(0)
	for _, file := range x.Files() {
		name := file.Path()
		if prior := a.files[name]; prior != nil {
			// Vendor installers can repeat artwork. Accept identical bounded
			// resources, while preserving a single unambiguous package inventory.
			if prior.Type.Value != xar.TypeFile || file.Type.Value != xar.TypeFile ||
				prior.Data == nil || file.Data == nil || len(prior.EAs)+len(file.EAs)+len(file.Children) != 0 ||
				prior.Data.Size != file.Data.Size || file.Data.Size > maxMetadata-duplicateBytes {
				return nil, fmt.Errorf("conflicting or oversized duplicate XAR path %q", name)
			}
			duplicateBytes += file.Data.Size
			var first, second bytes.Buffer
			if err := a.readFile(prior, &first, maxMetadata); err != nil {
				return nil, err
			}
			if err := a.readFile(file, &second, maxMetadata); err != nil {
				return nil, err
			}
			if !bytes.Equal(first.Bytes(), second.Bytes()) {
				return nil, fmt.Errorf("conflicting duplicate XAR path %q", name)
			}
			continue
		}
		a.files[name] = file
		entry := Entry{Path: name, Type: file.Type.Value}
		if file.Data != nil {
			entry.Size, entry.CompressedSize, entry.Encoding = file.Data.Size, file.Data.Length, file.Data.Encoding.Style
		}
		a.entries = append(a.entries, entry)
	}
	return a, nil
}

func validatePackageXAR(x *xar.Reader) error {
	if len(x.Files()) > 100000 {
		return fmt.Errorf("too many XAR entries")
	}
	for _, file := range x.Files() {
		name := file.Name()
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00:") {
			return fmt.Errorf("unsafe XAR filename %q", name)
		}
		if len(file.Children) != 0 && !file.IsDir() {
			return fmt.Errorf("XAR children in non-directory %q", file.Path())
		}
		if file.Data != nil {
			if file.Data.Size < 0 {
				return fmt.Errorf("invalid XAR data size for %s", file.Path())
			}
			if _, err := x.HeapSection(file.Data.Offset, file.Data.Length); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *xarArchive) readEntry(name string, dst io.Writer, limit int64) error {
	file := a.files[name]
	if file == nil || file.Type.Value != xar.TypeFile {
		return fmt.Errorf("XAR entry %q is not a regular file", name)
	}
	return a.readFile(file, dst, limit)
}

func (a *xarArchive) readFile(file *xar.File, dst io.Writer, limit int64) error {
	if file.Data == nil {
		return fmt.Errorf("%w: XAR regular file without data descriptor", ErrUnsupported)
	}
	if err := validatePackageData(file.Data, limit); err != nil {
		return err
	}
	r, err := a.reader.OpenVerified(file)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	_, err = io.Copy(dst, r)
	return err
}

func validatePackageData(data *xar.Data, limit int64) error {
	if data.Size < 0 || data.Size > limit || data.Length > maxEntrySize {
		return fmt.Errorf("XAR entry exceeds read limit")
	}
	return nil
}

func hasPackageChecksums(archived, extracted *xar.Digest) bool {
	return archived != nil && archived.Value != "" && extracted != nil && extracted.Value != ""
}

func (a *xarArchive) verifyEA(ea *xar.EA) error {
	if !hasPackageChecksums(ea.ArchivedChecksum, ea.ExtractedChecksum) {
		return fmt.Errorf("XAR extended attribute requires archived and extracted checksums")
	}
	if err := validatePackageData(&xar.Data{Size: ea.Size, Length: ea.Length, ArchivedChecksum: ea.ArchivedChecksum, ExtractedChecksum: ea.ExtractedChecksum}, maxEntrySize); err != nil {
		return err
	}
	r, err := a.reader.OpenEAVerified(ea)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	_, err = io.Copy(io.Discard, r)
	return err
}

func (a *xarArchive) verifySignature() (*x509.Certificate, error) {
	result, err := pkgsign.Verify(a.reader, pkgsign.VerifyOptions{AllowUntrusted: true})
	if err != nil {
		return nil, err
	}
	if !result.Valid() {
		return nil, fmt.Errorf("XAR signature invalid: %s", strings.Join(result.Errors, "; "))
	}
	return result.Signer, nil
}
