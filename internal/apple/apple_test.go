package apple

import (
	"bytes"
	"compress/zlib"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/signature"
	"howett.net/plist"
)

func TestAppFixtureInspection(t *testing.T) {
	facts, err := InspectApp("testdata/Fixture.app")
	if err != nil {
		t.Fatal(err)
	}
	if facts.BundleID != "au.edu.vic.woodleigh.stemma.fixture" || facts.Version != "1.2.3" || facts.Build != "42" {
		t.Fatalf("wrong app facts: %+v", facts)
	}
	macho, err := InspectMachO("testdata/Fixture.app/Contents/MacOS/fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(macho.Architectures) != 2 {
		t.Fatalf("expected two architecture signatures: %+v", macho)
	}
	for _, arch := range macho.Architectures {
		if !arch.AdHoc || arch.HasCMS || arch.Identifier != facts.BundleID {
			t.Fatalf("wrong signature facts: %+v", arch)
		}
	}
}

func TestSignedFixtures(t *testing.T) {
	app, err := VerifyApp(t.Context(), "testdata/SignedFixture.app", signature.Signer{})
	if err != nil || app.Signer != fixtureSigner || app.Name != "Woodleigh School" || app.Authority != "Developer ID Application" || app.Verifier != signature.Verifier {
		t.Fatalf("signed app: %+v: %v", app, err)
	}
	facts, err := InspectMachO("testdata/SignedFixture.app/Contents/MacOS/fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range facts.Architectures {
		if !arch.HasCMS || arch.AdHoc || arch.TeamID != "SMLKBTR495" {
			t.Fatalf("wrong company signature facts: %+v", arch)
		}
	}
	pkg, err := VerifyPackage(t.Context(), "testdata/fixture.pkg", signature.Signer{Scheme: signature.AppleDeveloperID, Value: "SMLKBTR495"})
	if err != nil || pkg.Signer != fixtureSigner || pkg.Name != "Woodleigh School" || pkg.Authority != "Developer ID Installer" || pkg.Target != "fixture.pkg" {
		t.Fatalf("company installer signature: %+v: %v", pkg, err)
	}
	if pkg, err := VerifyPackage(t.Context(), "testdata/fixture.pkg", signature.Signer{Scheme: signature.AppleDeveloperID, Value: "AAAAAAAAAA"}); !errors.Is(err, signature.ErrMismatch) || pkg.Signer != fixtureSigner {
		t.Fatalf("unexpected installer signer accepted: %+v: %v", pkg, err)
	}
}

func TestBinaryPlistMetadataAndExecutableTraversal(t *testing.T) {
	app := copyApp(t)
	facts := AppFacts{BundleID: "org.example.fixture", Name: "Binary", Version: "2.0", Build: "4", Executable: "fixture"}
	data, err := plist.Marshal(facts, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(app, "Contents/Info.plist"), data, 0644)
	got, err := InspectApp(app)
	if err != nil || got != facts {
		t.Fatalf("binary plist: %+v: %v", got, err)
	}
	facts.Executable = "../../escape"
	data, err = plist.Marshal(facts, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(app, "Contents/Info.plist"), data, 0644)
	if _, err := InspectApp(app); err == nil {
		t.Fatal("accepted executable traversal")
	}
}

func TestPackageXMLRejectsAmbiguity(t *testing.T) {
	for _, data := range []string{`<pkg-info><payload><size>0</size><size>20</size></payload></pkg-info>`, `<pkg-info identifier="a" identifier="b"/>`, `<pkg-info/><pkg-info/>`} {
		if err := validatePackageXML([]byte(data)); err == nil {
			t.Fatalf("accepted ambiguous XML %s", data)
		}
	}
}

func TestPackageInspectionAndIntegrity(t *testing.T) {
	before := readTestFile(t, "testdata/fixture.pkg")
	facts, err := InspectPackage("testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Packages) != 1 || facts.Packages[0].Identifier != "au.edu.vic.woodleigh.stemma.fixture" || facts.Packages[0].Version != "1.2.3" || facts.Packages[0].InstallLocation != "/Applications" {
		t.Fatalf("wrong package metadata: %+v", facts)
	}
	seenPayload := false
	for _, entry := range facts.Entries {
		if entry.Path == "Payload" {
			seenPayload = true
		}
	}
	if !seenPayload {
		t.Fatal("fixture payload was not retained as an archive entry")
	}
	if _, err := VerifyPackage(t.Context(), "testdata/fixture.pkg", signature.Signer{}); err != nil {
		t.Fatalf("native package: %v", err)
	}
	if !bytes.Equal(before, readTestFile(t, "testdata/fixture.pkg")) {
		t.Fatal("inspection changed installer bytes")
	}
	archive, err := openXAR(bytes.NewReader(before), int64(len(before)))
	if err != nil {
		t.Fatal(err)
	}
	offset := archive.reader.HeapOffset() + archive.files["Payload"].Data.Offset
	tampered := bytes.Clone(before)
	tampered[offset] ^= 0x40
	file := filepath.Join(t.TempDir(), "tampered.pkg")
	writeTestFile(t, file, tampered, 0644)
	if _, err := VerifyPackage(t.Context(), file, signature.Signer{}); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestPackageRequiresAppleAnchoredDeveloperID(t *testing.T) {
	archive := signedXAR(t, "PackageInfo")
	file := filepath.Join(t.TempDir(), "signed.pkg")
	writeTestFile(t, file, archive, 0644)
	if _, err := VerifyPackage(t.Context(), file, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "package signature") {
		t.Fatalf("self-signed package accepted: %v", err)
	}
}

func TestPackageRejectsCorruptCMSWithValidRSA(t *testing.T) {
	data := readTestFile(t, "testdata/fixture.pkg")
	archive, err := openXAR(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	toc := archive.reader.TOC()
	if toc.Signature == nil || toc.XSignature == nil {
		t.Fatal("fixture needs both RSA and CMS signatures")
	}
	// The CMS blob lives in the heap; changing it leaves the signed TOC and
	// the independent RSA signature untouched.
	data[archive.reader.HeapOffset()+toc.XSignature.Offset] ^= 1
	file := filepath.Join(t.TempDir(), "corrupt-cms.pkg")
	writeTestFile(t, file, data, 0o600)
	if _, err := VerifyPackage(t.Context(), file, signature.Signer{}); err == nil {
		t.Fatal("corrupt CMS accepted")
	}
}

func TestXARRejectsTraversalAndCorruptTOC(t *testing.T) {
	archive := signedXAR(t, "..")
	if _, err := openXAR(bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("accepted unsafe XAR filename")
	}
	archive = signedXAR(t, "PackageInfo")
	archive[30] ^= 1
	if _, err := openXAR(bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("accepted corrupt TOC")
	}
}

func copyApp(t *testing.T) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Fixture.app")
	if err := os.CopyFS(app, os.DirFS("testdata/Fixture.app")); err != nil {
		t.Fatal(err)
	}
	return app
}

// corruptExecutable flips a byte inside one architecture's sealed code pages.
func corruptExecutable(t *testing.T, file string, arch int) {
	t.Helper()
	data := readTestFile(t, file)
	entry := data[8+arch*20 : 28+arch*20]
	offset := binary.BigEndian.Uint32(entry[8:12])
	slice := data[offset : offset+binary.BigEndian.Uint32(entry[12:16])]
	commands := slice[32 : 32+binary.LittleEndian.Uint32(slice[20:24])]
	sealed := uint32(0)
	for range binary.LittleEndian.Uint32(slice[16:20]) {
		if binary.LittleEndian.Uint32(commands[:4]) == 0x1d {
			sealed = binary.LittleEndian.Uint32(commands[8:12])
		}
		commands = commands[binary.LittleEndian.Uint32(commands[4:8]):]
	}
	if sealed == 0 {
		t.Fatal("architecture is unsigned")
	}
	data[offset+sealed/2] ^= 0x40
	writeTestFile(t, file, data, 0755)
}

// portableOnly disables the platform verifier so the test exercises Stemma's own checks.
func portableOnly(t *testing.T) {
	t.Helper()
	native := nativeBundleValidity
	nativeBundleValidity = nil
	t.Cleanup(func() { nativeBundleValidity = native })
}

func readTestFile(t *testing.T, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestFile(t *testing.T, file string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(file, data, mode); err != nil {
		t.Fatal(err)
	}
}

func signedXAR(t *testing.T, name string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Stemma test signer"}, NotBefore: time.Unix(0, 0), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(`<pkg-info identifier="org.example.test" version="1" install-location="/Applications"/>`)
	contentHash := sha256.Sum256(content)
	xml := fmt.Sprintf(`<xar><toc><checksum style="sha256"><offset>0</offset><size>32</size></checksum><signature style="RSA"><offset>32</offset><size>256</size><KeyInfo><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></signature><file><name>%s</name><type>file</type><data><offset>288</offset><length>%d</length><size>%d</size><encoding style="application/octet-stream"/><archived-checksum style="sha256">%x</archived-checksum><extracted-checksum style="sha256">%x</extracted-checksum></data></file></toc></xar>`, base64.StdEncoding.EncodeToString(der), name, len(content), len(content), contentHash, contentHash)
	var compressed bytes.Buffer
	zr := zlib.NewWriter(&compressed)
	if _, err := zr.Write([]byte(xml)); err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(compressed.Bytes())
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 28)
	copy(header, "xar!")
	binary.BigEndian.PutUint16(header[4:6], 28)
	binary.BigEndian.PutUint16(header[6:8], 1)
	binary.BigEndian.PutUint64(header[8:16], uint64(compressed.Len()))
	binary.BigEndian.PutUint64(header[16:24], uint64(len(xml)))
	binary.BigEndian.PutUint32(header[24:28], 3)
	archive := header
	archive = append(archive, compressed.Bytes()...)
	archive = append(archive, digest[:]...)
	archive = append(archive, signature...)
	archive = append(archive, content...)
	return archive
}
