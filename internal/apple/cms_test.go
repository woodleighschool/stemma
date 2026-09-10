package apple

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"
	"howett.net/plist"
)

func TestCMSAuthenticatesEveryArchitecture(t *testing.T) {
	for _, arch := range []int{0, 1} {
		t.Run([]string{"first", "second"}[arch], func(t *testing.T) {
			signature := signedFixtureSignature(t, arch)
			signed, err := pkcs7.Parse(signature.blobs[0x10000][8:])
			if err != nil {
				t.Fatal(err)
			}
			app := filepath.Join(t.TempDir(), "SignedFixture.app")
			if err := os.CopyFS(app, os.DirFS("testdata/SignedFixture.app")); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(app, "Contents/MacOS/fixture")
			data := readTestFile(t, executable)
			offset := bytes.Index(data, signed.Signers[0].EncryptedDigest)
			if offset < 0 {
				t.Fatal("fixture CMS signature is missing")
			}
			data[offset] ^= 0x40
			writeTestFile(t, executable, data, 0o755)
			evidence, err := VerifyApp(app, Policy{RequireSignature: true})
			if err == nil || evidence.Integrity.Status != Valid || evidence.Signature.Status != Invalid || evidence.Resources.Status != NotRequested {
				t.Fatalf("altered CMS signature accepted or scopes collapsed: %+v: %v", evidence, err)
			}
		})
	}
}

func TestCMSSignatureAllocationStillBindsArtifactIdentity(t *testing.T) {
	app := filepath.Join(t.TempDir(), "SignedFixture.app")
	if err := os.CopyFS(app, os.DirFS("testdata/SignedFixture.app")); err != nil {
		t.Fatal(err)
	}
	baseline, err := VerifyApp(app, Policy{RequireSignature: true})
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(app, "Contents/MacOS/fixture")
	data := readTestFile(t, executable)
	// The fat header gives the first architecture's offset and allocated size.
	base := int(binary.BigEndian.Uint32(data[16:20]))
	size := int(binary.BigEndian.Uint32(data[20:24]))
	start := base + int(signedFixtureSignature(t, 0).codeOffset)
	end := start + int(binary.BigEndian.Uint32(data[start+4:start+8]))
	if end >= base+size {
		t.Fatal("fixture has no unused signature allocation")
	}
	data[end] ^= 0x40
	writeTestFile(t, executable, data, 0o755)
	evidence, err := VerifyApp(app, Policy{RequireSignature: true})
	if err != nil || evidence.Integrity.Status != Valid || evidence.Signature.Status != Valid || evidence.SubjectSHA256 == baseline.SubjectSHA256 {
		t.Fatalf("signature allocation confused authentication and tree identity: %+v: %v", evidence, err)
	}
}

func TestCMSRejectsUnboundedAndTrailingEncoding(t *testing.T) {
	for _, name := range []string{"trailing", "depth", "indefinite-depth", "count", "size", "unterminated", "truncated-eoc", "unexpected-eoc", "primitive-indefinite"} {
		t.Run(name, func(t *testing.T) {
			signature := signedFixtureSignature(t, 0)
			data := bytes.Clone(signature.blobs[0x10000][8:])
			switch name {
			case "trailing":
				data = append(data, 5, 0)
			case "depth":
				data = []byte{5, 0}
				for range 34 {
					var err error
					data, err = asn1.Marshal(asn1.RawValue{Tag: asn1.TagSequence, IsCompound: true, Bytes: data})
					if err != nil {
						t.Fatal(err)
					}
				}
			case "count":
				var err error
				data, err = asn1.Marshal(asn1.RawValue{Tag: asn1.TagSequence, IsCompound: true, Bytes: bytes.Repeat([]byte{5, 0}, 16385)})
				if err != nil {
					t.Fatal(err)
				}
			case "size":
				data = make([]byte, 1<<20+1)
			case "indefinite-depth":
				data = append(bytes.Repeat([]byte{0x30, 0x80}, 34), bytes.Repeat([]byte{0, 0}, 34)...)
			case "unterminated":
				data = []byte{0x30, 0x80, 5, 0}
			case "truncated-eoc":
				data = []byte{0x30, 0x80, 5, 0, 0}
			case "unexpected-eoc":
				data = []byte{0x30, 2, 0, 0}
			case "primitive-indefinite":
				data = []byte{0x04, 0x80, 0, 0}
			}
			signature.blobs[0x10000] = append(signature.blobs[0x10000][:8:8], data...)
			if _, err := verifyCMS(signature); err == nil || !strings.Contains(err.Error(), "encoding") {
				t.Fatalf("unsafe CMS encoding reached the CMS parser: %v", err)
			}
		})
	}
}

func TestCMSSignerPinDoesNotClaimPlatformTrust(t *testing.T) {
	signature := signedFixtureSignature(t, 0)
	certificate, err := verifyCMS(signature)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(certificate.Raw)
	for _, pin := range []string{hex.EncodeToString(digest[:]), strings.Repeat("0", 64)} {
		evidence, err := VerifyApp("testdata/SignedFixture.app", Policy{CertificateSHA256: pin})
		valid := pin != strings.Repeat("0", 64)
		if (err == nil) != valid || (evidence.Identity.Status == Valid) != valid || evidence.Signature.Status != Valid || evidence.Integrity.Status != Valid || evidence.Platform.Status != NotRequested || evidence.Resources.Status != NotRequested {
			t.Fatalf("wrong certificate pin scope: %+v: %v", evidence, err)
		}
	}
	evidence, err := VerifyApp("testdata/SignedFixture.app", Policy{RequireSignature: true, RequireIdentity: true, RequirePlatform: true})
	if !errors.Is(err, ErrUnsupported) || evidence.Signature.Status != Valid || evidence.Identity.Status != Unsupported || evidence.Platform.Status != Unsupported {
		t.Fatalf("CMS authentication claimed unspecified trust: %+v: %v", evidence, err)
	}
}

func TestCMSRejectsUnboundCodeDirectories(t *testing.T) {
	t.Run("modified-primary", func(t *testing.T) {
		signature := signedFixtureSignature(t, 0)
		cd := signature.directories[0]
		cd[len(cd)-1] ^= 1
		if _, err := verifyCMS(signature); err == nil {
			t.Fatal("CMS authenticated a changed primary CodeDirectory")
		}
	})
	t.Run("alternate", func(t *testing.T) {
		signature := signedFixtureSignature(t, 0)
		signature.directories = append(signature.directories, bytes.Clone(signature.directories[0]))
		if _, err := verifyCMS(signature); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("alternate CodeDirectory authentication was claimed: %v", err)
		}
	})
	for _, version := range []int{1, 2} {
		t.Run([]string{"", "agility-v1", "agility-v2"}[version], func(t *testing.T) {
			signature := signedFixtureSignature(t, 0)
			attribute := pkcs7.Attribute{Type: oidHashAgilityV2, Value: struct {
				Algorithm asn1.ObjectIdentifier
				Digest    []byte
			}{pkcs7.OIDDigestAlgorithmSHA256, make([]byte, 32)}}
			if version == 1 {
				data, err := plist.Marshal(map[string]any{"cdhashes": [][]byte{make([]byte, 20)}}, plist.XMLFormat)
				if err != nil {
					t.Fatal(err)
				}
				attribute = pkcs7.Attribute{Type: oidHashAgility, Value: data}
			}
			signature.blobs[0x10000] = signedCMS(t, signature.directories[0], pkcs7.OIDDigestAlgorithmSHA256, attribute)
			if _, err := verifyCMS(signature); err == nil || !strings.Contains(err.Error(), "hash agility") {
				t.Fatalf("signed mismatching agility digest was accepted: %v", err)
			}
		})
	}
	t.Run("weak-cms-digest", func(t *testing.T) {
		signature := signedFixtureSignature(t, 0)
		signature.blobs[0x10000] = signedCMS(t, signature.directories[0], pkcs7.OIDDigestAlgorithmSHA1)
		if _, err := verifyCMS(signature); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("weak CMS digest authenticated a SHA-256 CodeDirectory: %v", err)
		}
	})
}

func signedFixtureSignature(t *testing.T, arch int) *codeSignature {
	t.Helper()
	f, err := os.Open("testdata/SignedFixture.app/Contents/MacOS/fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	slices, err := machoSlices(f, info.Size())
	if err != nil || arch >= len(slices) {
		t.Fatalf("missing fixture architecture: %v", err)
	}
	signature, err := slices[arch].signature()
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

func signedCMS(t *testing.T, content []byte, digest asn1.ObjectIdentifier, attributes ...pkcs7.Attribute) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := pkcs7.NewSignedData(content)
	if err != nil {
		t.Fatal(err)
	}
	signed.SetDigestAlgorithm(digest)
	if err := signed.AddSigner(certificate, key, pkcs7.SignerInfoConfig{ExtraSignedAttributes: attributes}); err != nil {
		t.Fatal(err)
	}
	signed.Detach()
	data, err := signed.Finish()
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 8, len(data)+8)
	binary.BigEndian.PutUint32(blob, 0xfade0b01)
	binary.BigEndian.PutUint32(blob[4:], uint32(len(data)+8))
	return append(blob, data...)
}
