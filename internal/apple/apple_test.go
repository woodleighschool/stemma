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
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

func TestAppFixtureIntegrity(t *testing.T) {
	facts, err := InspectApp("testdata/Fixture.app")
	if err != nil {
		t.Fatal(err)
	}
	if facts.BundleID != "au.edu.vic.woodleigh.stemma.fixture" || facts.Version != "1.2.3" || facts.Build != "42" {
		t.Fatalf("wrong app facts: %+v", facts)
	}
	evidence, err := VerifyApp("testdata/Fixture.app", Policy{RequireIntegrity: true, RequireResources: true})
	if err != nil {
		t.Fatalf("verification failed: %+v: %v", evidence, err)
	}
	if evidence.Integrity.Status != Valid || evidence.Resources.Status != Valid || evidence.Signature.Status != NotRequested || evidence.Identity.Status != NotRequested {
		t.Fatalf("wrong evidence: %+v", evidence)
	}
	if len(evidence.SubjectSHA256) != 64 || len(evidence.PolicySHA256) != 64 || evidence.Verifier != Verifier {
		t.Fatalf("unbound evidence: %+v", evidence)
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
	const installerPin = "e8bcd85f5b71188453845541f51b08e49f7262f10f3d79f5fe942a6735ae9760"
	app, err := VerifyApp("testdata/SignedFixture.app", Policy{RequireIntegrity: true, RequireResources: true})
	if err != nil || app.Integrity.Status != Valid || app.Resources.Status != Valid {
		t.Fatalf("signed app integrity: %+v: %v", app, err)
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
	app, err = VerifyApp("testdata/SignedFixture.app", Policy{RequireSignature: true, RequireResources: true})
	if !errors.Is(err, ErrUnsupported) || app.Signature.Status != Unsupported || app.Integrity.Status != Valid {
		t.Fatalf("CMS parsing was promoted to verification: %+v: %v", app, err)
	}
	pkg, err := VerifyPackage("testdata/fixture.pkg", Policy{RequireSignature: true, CertificateSHA256: installerPin})
	if err != nil || pkg.Signature.Status != Valid || pkg.Identity.Status != Valid || pkg.Integrity.Status != Valid {
		t.Fatalf("company installer signature: %+v: %v", pkg, err)
	}
}

func TestAppRejectsTamperingAndUnsupportedScopes(t *testing.T) {
	for _, mutation := range []struct {
		name        string
		apply       func(*testing.T, string)
		unsupported bool
	}{
		{"resource", func(t *testing.T, app string) {
			writeTestFile(t, filepath.Join(app, "Contents/Resources/message.txt"), []byte("modified"), 0644)
		}, false},
		{"executable", func(t *testing.T, app string) { corruptExecutable(t, filepath.Join(app, "Contents/MacOS/fixture"), 0) }, false},
		{"second_architecture", func(t *testing.T, app string) { corruptExecutable(t, filepath.Join(app, "Contents/MacOS/fixture"), 1) }, false},
		{"info", func(t *testing.T, app string) {
			f := filepath.Join(app, "Contents/Info.plist")
			data := readTestFile(t, f)
			writeTestFile(t, f, bytes.ReplaceAll(data, []byte("1.2.3"), []byte("9.8.7")), 0644)
		}, false},
		{"unsealed_file", func(t *testing.T, app string) {
			writeTestFile(t, filepath.Join(app, "Contents/Resources/extra.txt"), []byte("unsealed"), 0644)
		}, false},
		{"symlink", func(t *testing.T, app string) {
			if err := os.Symlink("message.txt", filepath.Join(app, "Contents/Resources/link")); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			app := copyApp(t)
			mutation.apply(t, app)
			evidence, err := VerifyApp(app, Policy{RequireIntegrity: true, RequireResources: true})
			if err == nil {
				t.Fatalf("tampered app passed: %+v", evidence)
			}
			if mutation.unsupported != errors.Is(err, ErrUnsupported) {
				t.Fatalf("wrong error scope: %+v: %v", evidence, err)
			}
			if evidence.Resources.Status == Valid {
				t.Fatalf("tampered resources passed: %+v", evidence)
			}
		})
	}
	t.Run("authentication", func(t *testing.T) {
		evidence, err := VerifyApp("testdata/Fixture.app", Policy{RequireIntegrity: true, RequireResources: true, RequireSignature: true, RequireIdentity: true, RequirePlatform: true})
		if err == nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported trust accepted: %+v: %v", evidence, err)
		}
		if evidence.Integrity.Status != Valid || evidence.Resources.Status != Valid || evidence.Signature.Status != Invalid || evidence.Identity.Status != Unsupported || evidence.Platform.Status != Unsupported {
			t.Fatalf("mixed scopes lost: %+v", evidence)
		}
	})
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

func TestResourceScopeRejectsNestedCode(t *testing.T) {
	root, err := os.OpenRoot("testdata/Fixture.app")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := plist.Marshal(map[string]any{"files2": map[string]any{"Frameworks/Nested.framework": map[string]any{"cdhash": make([]byte, 20), "requirement": "identifier org.example.nested"}}}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyResources(root, "Contents/MacOS/fixture", data); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nested code did not block resource scope: %v", err)
	}
}

func TestXARRejectsAmbiguousXML(t *testing.T) {
	for _, data := range []string{`<xar><toc><checksum><offset>0</offset><offset>20</offset></checksum></toc></xar>`, `<xar><toc/><toc/></xar>`, `<xar><toc><encoding style="a" style="b"/></toc></xar>`, `<xar/><xar/>`} {
		if err := validateTOCXML([]byte(data)); err == nil {
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
	evidence, err := VerifyPackage("testdata/fixture.pkg", Policy{RequireIntegrity: true})
	if err != nil || evidence.Integrity.Status != Valid {
		t.Fatalf("native package integrity: %+v: %v", evidence, err)
	}
	if !bytes.Equal(before, readTestFile(t, "testdata/fixture.pkg")) {
		t.Fatal("inspection changed installer bytes")
	}
	archive, err := openXAR(bytes.NewReader(before), int64(len(before)))
	if err != nil {
		t.Fatal(err)
	}
	offset := archive.heap + archive.files["Payload"].Data.Offset
	tampered := bytes.Clone(before)
	tampered[offset] ^= 0x40
	file := filepath.Join(t.TempDir(), "tampered.pkg")
	writeTestFile(t, file, tampered, 0644)
	if evidence, err := VerifyPackage(file, Policy{RequireIntegrity: true}); err == nil || evidence.Integrity.Status != Invalid {
		t.Fatalf("tampered payload accepted: %+v: %v", evidence, err)
	}
}

func TestPackageRSASignatureAndPinnedIdentity(t *testing.T) {
	archive, pin := signedXAR(t, "PackageInfo")
	file := filepath.Join(t.TempDir(), "signed.pkg")
	writeTestFile(t, file, archive, 0644)
	policy := Policy{RequireIntegrity: true, RequireSignature: true, RequireIdentity: true, CertificateSHA256: pin}
	evidence, err := VerifyPackage(file, policy)
	if err != nil || evidence.Integrity.Status != Valid || evidence.Signature.Status != Valid || evidence.Identity.Status != Valid {
		t.Fatalf("signed fixture: %+v: %v", evidence, err)
	}
	policy.CertificateSHA256 = strings.Repeat("0", 64)
	if evidence, err := VerifyPackage(file, policy); err == nil || evidence.Identity.Status != Invalid || evidence.Signature.Status != Valid {
		t.Fatalf("wrong signer pin accepted: %+v: %v", evidence, err)
	}
	policy.CertificateSHA256 = ""
	if evidence, err := VerifyPackage(file, policy); !errors.Is(err, ErrUnsupported) || evidence.Identity.Status != Unsupported {
		t.Fatalf("implicit trust accepted: %+v: %v", evidence, err)
	}
	policy.CertificateSHA256 = pin
	toc, err := openXAR(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	archive[toc.heap+toc.toc.Signatures[0].Offset] ^= 1
	writeTestFile(t, file, archive, 0644)
	if evidence, err := VerifyPackage(file, policy); err == nil || evidence.Signature.Status != Invalid || evidence.Identity.Status != Invalid {
		t.Fatalf("tampered RSA signature accepted: %+v: %v", evidence, err)
	}
}

func TestXARRejectsTraversalAndCorruptTOC(t *testing.T) {
	archive, _ := signedXAR(t, "..")
	if _, err := openXAR(bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("accepted unsafe XAR filename")
	}
	archive, _ = signedXAR(t, "PackageInfo")
	archive[30] ^= 1
	if _, err := openXAR(bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("accepted corrupt TOC")
	}
}

func TestXARAllowsOnlyIdenticalRepeatedNames(t *testing.T) {
	archive := &xarArchive{files: make(map[string]xarFile)}
	if err := archive.addFiles("", []xarFile{{Names: []string{"Payload", "Payload"}, Type: "directory"}}, 0); err != nil {
		t.Fatalf("identical repeated vendor filename rejected: %v", err)
	}
	archive = &xarArchive{files: make(map[string]xarFile)}
	if err := archive.addFiles("", []xarFile{{Names: []string{"Payload", "Scripts"}, Type: "directory"}}, 0); err == nil {
		t.Fatal("contradictory XAR filenames accepted")
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

func corruptExecutable(t *testing.T, file string, arch int) {
	t.Helper()
	data := readTestFile(t, file)
	entry := data[8+arch*20 : 28+arch*20]
	offset := binary.BigEndian.Uint32(entry[8:12])
	length := binary.BigEndian.Uint32(entry[12:16])
	// The byte lies in the slice's signed region, after load commands.
	data[int(offset)+int(length)/2] ^= 0x40
	writeTestFile(t, file, data, 0755)
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

func signedXAR(t *testing.T, name string) ([]byte, string) {
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
	pin := sha256.Sum256(der)
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
	return archive, hex.EncodeToString(pin[:])
}
