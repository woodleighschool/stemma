package apple

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"
	"github.com/woodleighschool/stemma/internal/signature"
	"howett.net/plist"
)

func TestCMSAuthenticatesEveryArchitecture(t *testing.T) {
	portableOnly(t)
	for _, arch := range []int{0, 1} {
		t.Run([]string{"first", "second"}[arch], func(t *testing.T) {
			sig := signedFixtureSignature(t, arch)
			signed, err := pkcs7.Parse(sig.blobs[0x10000][8:])
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
			if _, err := VerifyApp(t.Context(), app, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "CMS signature") {
				t.Fatalf("altered CMS signature accepted: %v", err)
			}
		})
	}
}

func TestCMSSignatureAllocationIsNotCode(t *testing.T) {
	portableOnly(t)
	app := filepath.Join(t.TempDir(), "SignedFixture.app")
	if err := os.CopyFS(app, os.DirFS("testdata/SignedFixture.app")); err != nil {
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
	if _, err := VerifyApp(t.Context(), app, signature.Signer{}); err != nil {
		t.Fatalf("unused signature allocation failed verification: %v", err)
	}
}

func TestCMSRejectsUnboundedAndTrailingEncoding(t *testing.T) {
	for _, name := range []string{"trailing", "depth", "indefinite-depth", "count", "size", "unterminated", "truncated-eoc", "unexpected-eoc", "primitive-indefinite"} {
		t.Run(name, func(t *testing.T) {
			sig := signedFixtureSignature(t, 0)
			data := bytes.Clone(sig.blobs[0x10000][8:])
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
			sig.blobs[0x10000] = append(sig.blobs[0x10000][:8:8], data...)
			if _, err := verifyCMS(sig); err == nil || !strings.Contains(err.Error(), "encoding") {
				t.Fatalf("unsafe CMS encoding reached the CMS parser: %v", err)
			}
		})
	}
}

func TestCMSChainMustAnchorAtApple(t *testing.T) {
	sig := signedFixtureSignature(t, 0)
	cms, err := verifyCMS(sig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identify(cms.certificate, cms.certificates, time.Time{}); err != nil {
		t.Fatalf("Developer ID chain rejected: %v", err)
	}
	// A signature by a certificate outside Apple's hierarchy authenticates the
	// CodeDirectory but establishes no signer.
	sig.blobs[0x10000] = signedCMS(t, sig.directories[0], pkcs7.OIDDigestAlgorithmSHA256)
	cms, err = verifyCMS(sig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identify(cms.certificate, cms.certificates, time.Time{}); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("self-signed CMS certificate established a signer: %v", err)
	}
	if _, err := trustedTime(nil, cms.signatureValue); err != nil {
		t.Fatalf("absent timestamp: %v", err)
	}
}

func TestCMSRejectsUnboundCodeDirectories(t *testing.T) {
	t.Run("modified-primary", func(t *testing.T) {
		sig := signedFixtureSignature(t, 0)
		cd := sig.directories[0]
		cd[len(cd)-1] ^= 1
		if _, err := verifyCMS(sig); err == nil {
			t.Fatal("CMS authenticated a changed primary CodeDirectory")
		}
	})
	t.Run("alternate", func(t *testing.T) {
		sig := signedFixtureSignature(t, 0)
		sig.directories = append(sig.directories, bytes.Clone(sig.directories[0]))
		if _, err := verifyCMS(sig); err == nil {
			t.Fatalf("alternate CodeDirectory authentication was claimed: %v", err)
		}
	})
	for _, version := range []int{1, 2} {
		t.Run([]string{"", "agility-v1", "agility-v2"}[version], func(t *testing.T) {
			sig := signedFixtureSignature(t, 0)
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
			sig.blobs[0x10000] = signedCMS(t, sig.directories[0], pkcs7.OIDDigestAlgorithmSHA256, attribute)
			if _, err := verifyCMS(sig); err == nil || !strings.Contains(err.Error(), "hash agility") {
				t.Fatalf("signed mismatching agility digest was accepted: %v", err)
			}
		})
	}
	t.Run("weak-cms-digest", func(t *testing.T) {
		sig := signedFixtureSignature(t, 0)
		sig.blobs[0x10000] = signedCMS(t, sig.directories[0], pkcs7.OIDDigestAlgorithmSHA1)
		if _, err := verifyCMS(sig); !errors.Is(err, ErrUnsupported) {
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
	sig, err := slices[arch].signature()
	if err != nil {
		t.Fatal(err)
	}
	return sig
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

func TestCMSBindsAlternateCodeDirectories(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			sig := signedFixtureSignature(t, 0)
			alternate := bytes.Clone(sig.directories[0])
			alternate[len(alternate)-1] ^= 1
			sig.directories = append(sig.directories, alternate)
			var short [][]byte
			var values []byte
			for _, cd := range sig.directories {
				digest := sha256.Sum256(cd)
				short = append(short, digest[:20])
				data, err := asn1.Marshal(struct {
					Algorithm asn1.ObjectIdentifier
					Digest    []byte
				}{pkcs7.OIDDigestAlgorithmSHA256, digest[:]})
				if err != nil {
					t.Fatal(err)
				}
				values = append(values, data...)
			}
			attribute := pkcs7.Attribute{Type: oidHashAgilityV2, Value: asn1.RawValue{FullBytes: values}}
			if version == 1 {
				data, err := plist.Marshal(map[string]any{"cdhashes": short}, plist.XMLFormat)
				if err != nil {
					t.Fatal(err)
				}
				attribute = pkcs7.Attribute{Type: oidHashAgility, Value: data}
			}
			sig.blobs[0x10000] = signedCMS(t, sig.directories[0], pkcs7.OIDDigestAlgorithmSHA256, attribute)
			if _, err := verifyCMS(sig); err != nil {
				t.Fatal(err)
			}
			alternate[len(alternate)-1] ^= 2
			if _, err := verifyCMS(sig); err == nil {
				t.Fatal("CMS accepted a substituted alternate directory")
			}
		})
	}
}
