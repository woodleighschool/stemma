package authenticode

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sassoftware/relic/v8/lib/authenticode"
	"github.com/sassoftware/relic/v8/lib/certloader"
	"github.com/sassoftware/relic/v8/lib/comdoc"
	"github.com/woodleighschool/stemma/internal/signature"
)

// authority is an issuing certification authority beneath a self-signed root,
// as commercial code-signing authorities are. Signatures embed the issuing
// certificate but not the root.
type authority struct {
	root        *x509.Certificate
	certificate *x509.Certificate
	key         *rsa.PrivateKey
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	const name = "Example Code Signing CA"
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootTemplate := *ca
	rootTemplate.Subject = pkix.Name{CommonName: name + " Root"}
	rootDER, err := x509.CreateCertificate(rand.Reader, &rootTemplate, &rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return authority{root, certificate, key}
}

// issue returns a code-signing identity for publisher from the authority.
func (a authority) issue(t *testing.T, publisher pkix.Name, serial int64) *certloader.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: publisher, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &certloader.Certificate{Leaf: leaf, Certificates: []*x509.Certificate{leaf, a.certificate, a.root}, PrivateKey: key}
}

var examplePublisher = pkix.Name{CommonName: "Example Publisher Pty Ltd", Organization: []string{"Example Publisher Pty Ltd"}, Locality: []string{"Melbourne"}, Country: []string{"AU"}, SerialNumber: "123456"}

func signMSI(t *testing.T, identity *certloader.Certificate) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "signed.msi")
	data, err := os.ReadFile("../msi/testdata/test.msi")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cdf, err := comdoc.WritePath(name)
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := authenticode.DigestMSI(cdf, crypto.SHA256, false)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := authenticode.SignMSIImprint(context.Background(), digest, crypto.SHA256, identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticode.InsertMSISignature(cdf, signed.Raw, nil); err != nil {
		t.Fatal(err)
	}
	if err := cdf.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

// minimalPE is a valid PE32+ image with one code section and no signature.
func minimalPE() []byte {
	image := make([]byte, 0x400)
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[0x3c:], 0x40)
	copy(image[0x40:], "PE\x00\x00")
	coff := image[0x44:]
	binary.LittleEndian.PutUint16(coff[0:], 0x8664)
	binary.LittleEndian.PutUint16(coff[2:], 1)
	binary.LittleEndian.PutUint16(coff[16:], 240)
	binary.LittleEndian.PutUint16(coff[18:], 0x22)
	optional := image[0x58:]
	binary.LittleEndian.PutUint16(optional[0:], 0x20b)
	binary.LittleEndian.PutUint32(optional[4:], 0x200)
	binary.LittleEndian.PutUint32(optional[16:], 0x1000)
	binary.LittleEndian.PutUint32(optional[20:], 0x1000)
	binary.LittleEndian.PutUint64(optional[24:], 0x140000000)
	binary.LittleEndian.PutUint32(optional[32:], 0x1000)
	binary.LittleEndian.PutUint32(optional[36:], 0x200)
	binary.LittleEndian.PutUint16(optional[40:], 6)
	binary.LittleEndian.PutUint16(optional[48:], 6)
	binary.LittleEndian.PutUint32(optional[56:], 0x2000)
	binary.LittleEndian.PutUint32(optional[60:], 0x200)
	binary.LittleEndian.PutUint16(optional[68:], 3)
	binary.LittleEndian.PutUint64(optional[72:], 0x100000)
	binary.LittleEndian.PutUint64(optional[80:], 0x1000)
	binary.LittleEndian.PutUint64(optional[88:], 0x100000)
	binary.LittleEndian.PutUint64(optional[96:], 0x1000)
	binary.LittleEndian.PutUint32(optional[108:], 16)
	section := image[0x58+240:]
	copy(section, ".text")
	binary.LittleEndian.PutUint32(section[8:], 0x200)
	binary.LittleEndian.PutUint32(section[12:], 0x1000)
	binary.LittleEndian.PutUint32(section[16:], 0x200)
	binary.LittleEndian.PutUint32(section[20:], 0x200)
	binary.LittleEndian.PutUint32(section[36:], 0x60000020)
	image[0x200] = 0xc3
	return image
}

func signPE(t *testing.T, identity *certloader.Certificate) string {
	t.Helper()
	dir := t.TempDir()
	unsigned := filepath.Join(dir, "setup.exe")
	if err := os.WriteFile(unsigned, minimalPE(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	digest, err := authenticode.DigestPE(f, crypto.SHA256, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, _, err := digest.Sign(context.Background(), identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	signed := filepath.Join(dir, "signed.exe")
	if err := patch.Apply(f, signed); err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestVerifyDerivesStablePublisherIdentity(t *testing.T) {
	ca := newAuthority(t)
	first := ca.issue(t, examplePublisher, 100)
	for name, sign := range map[string]func(*testing.T, *certloader.Certificate) string{"msi": signMSI, "exe": signPE} {
		t.Run(name, func(t *testing.T) {
			result, err := Verify(t.Context(), sign(t, first), signature.Signer{})
			if err != nil {
				t.Fatal(err)
			}
			signer, parseErr := signature.Parse(result.Signer)
			if parseErr != nil || signer.Scheme != signature.Authenticode || result.Name != examplePublisher.CommonName || result.Authority != "Example Code Signing CA" || result.Verifier != signature.Verifier {
				t.Fatalf("derived result: %+v: %v", result, parseErr)
			}
			// Routine renewal keeps the same identity.
			renewed, err := Verify(t.Context(), sign(t, ca.issue(t, examplePublisher, 101)), signer)
			if err != nil || renewed.Signer != result.Signer {
				t.Fatalf("renewed certificate changed the signer: %+v: %v", renewed, err)
			}
			// A different authority or a different publisher is a new identity.
			other := newAuthority(t)
			if _, err := Verify(t.Context(), sign(t, other.issue(t, examplePublisher, 100)), signer); !errors.Is(err, signature.ErrMismatch) {
				t.Fatalf("new authority key kept the signer: %v", err)
			}
			changed := examplePublisher
			changed.CommonName = "Example Publisher Ltd"
			if _, err := Verify(t.Context(), sign(t, ca.issue(t, changed, 102)), signer); !errors.Is(err, signature.ErrMismatch) {
				t.Fatalf("changed publisher kept the signer: %v", err)
			}
			if _, err := Verify(t.Context(), sign(t, first), signature.Signer{Scheme: signature.Authenticode, Value: strings.Repeat("0", 64)}); !errors.Is(err, signature.ErrMismatch) {
				t.Fatalf("mismatch was not reported: %v", err)
			}
		})
	}
}

func TestVerifyRejectsTamperedAndUnsignedInstallers(t *testing.T) {
	ca := newAuthority(t)
	identity := ca.issue(t, examplePublisher, 100)
	for name, sign := range map[string]func(*testing.T, *certloader.Certificate) string{"msi": signMSI, "exe": signPE} {
		t.Run(name, func(t *testing.T) {
			file := sign(t, identity)
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			tampered := append([]byte(nil), data...)
			// Signed content lies before the appended signature.
			tampered[0x210] ^= 1
			if err := os.WriteFile(file, tampered, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(t.Context(), file, signature.Signer{}); err == nil {
				t.Fatal("tampered installer verified")
			}
		})
	}
	unsigned := filepath.Join(t.TempDir(), "setup.exe")
	if err := os.WriteFile(unsigned, minimalPE(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), unsigned, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned installer: %v", err)
	}
	if _, err := Verify(t.Context(), "../msi/testdata/test.msi", signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned MSI: %v", err)
	}
	if _, err := Verify(t.Context(), "setup.msix", signature.Signer{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported format: %v", err)
	}
}

func TestVerifyRequiresEmbeddedAuthority(t *testing.T) {
	ca := newAuthority(t)
	identity := ca.issue(t, examplePublisher, 100)
	identity.Certificates = identity.Certificates[:1]
	if _, err := Verify(t.Context(), signMSI(t, identity), signature.Signer{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing issuing authority accepted: %v", err)
	}
}
