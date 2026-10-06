package apple

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deploymenttheory/go-macos-pkg/pkg/pkgsign"
	"github.com/smallstep/pkcs7"
	"github.com/woodleighschool/stemma/internal/signature"
)

// testKey signs every synthetic certificate and signature. Generating a key
// for each would dominate these tests.
var testKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

// hierarchy stands in for Apple's certificates, whose keys only Apple holds:
// a root, an issuing authority, a signing certificate and a timestamp
// authority.
type hierarchy struct {
	roots     []*x509.Certificate
	authority *x509.Certificate
	leaf      *x509.Certificate
	timestamp *x509.Certificate
	key       *rsa.PrivateKey
}

// certify returns template as a certificate that parent issued under the test
// key.
func certify(t *testing.T, template, parent *x509.Certificate) *x509.Certificate {
	t.Helper()
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

// issue builds a hierarchy. As under Apple's roots, the markers on the
// authority and the signing certificate decide the signing class.
func issue(t *testing.T, authorityMarker, purpose asn1.ObjectIdentifier, subject pkix.Name, notAfter time.Time) hierarchy {
	t.Helper()
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	marked := func(id asn1.ObjectIdentifier) []pkix.Extension {
		if id == nil {
			return nil
		}
		return []pkix.Extension{{Id: id, Value: []byte{5, 0}}}
	}
	decade := 10 * 365 * 24 * time.Hour
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root"}, NotBefore: time.Now().Add(-decade), NotAfter: time.Now().Add(decade), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	root := certify(t, rootTemplate, rootTemplate)
	authority := certify(t, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Test Authority"}, NotBefore: root.NotBefore, NotAfter: root.NotAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, ExtraExtensions: marked(authorityMarker)}, root)
	leaf := certify(t, &x509.Certificate{SerialNumber: big.NewInt(3), Subject: subject, NotBefore: notAfter.Add(-2 * 365 * 24 * time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, ExtraExtensions: marked(purpose)}, authority)
	timestamp := certify(t, &x509.Certificate{SerialNumber: big.NewInt(4), Subject: pkix.Name{CommonName: "Test Timestamp Authority"}, NotBefore: root.NotBefore, NotAfter: root.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}}, root)
	return hierarchy{roots: []*x509.Certificate{root}, authority: authority, leaf: leaf, timestamp: timestamp, key: key}
}

// stamp returns an RFC 3161 token in which the hierarchy's timestamp
// authority attests that a signature value existed at a time.
func (h hierarchy) stamp(t *testing.T, signed []byte, at time.Time) []byte {
	t.Helper()
	imprint := sha256.Sum256(signed)
	info, err := asn1.Marshal(struct {
		Version int
		Policy  asn1.ObjectIdentifier
		Imprint struct {
			Algorithm pkix.AlgorithmIdentifier
			Digest    []byte
		}
		Serial *big.Int
		Time   time.Time `asn1:"generalized"`
	}{Version: 1, Policy: asn1.ObjectIdentifier{1, 2, 3}, Imprint: struct {
		Algorithm pkix.AlgorithmIdentifier
		Digest    []byte
	}{pkix.AlgorithmIdentifier{Algorithm: pkcs7.OIDDigestAlgorithmSHA256}, imprint[:]}, Serial: big.NewInt(1), Time: at.UTC().Truncate(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	token, err := pkcs7.NewSignedData(info)
	if err != nil {
		t.Fatal(err)
	}
	token.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	// A token's content is a TSTInfo.
	token.GetSignedData().ContentInfo.ContentType = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	if err := token.AddSigner(h.timestamp, h.key, pkcs7.SignerInfoConfig{}); err != nil {
		t.Fatal(err)
	}
	der, err := token.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// resign replaces the CMS signature in every architecture of a signed Mach-O
// with the hierarchy's, dated within its signing certificate's life. timestamp
// returns the token an architecture's signature value carries, if any. A
// CodeDirectory keeps its bytes unless edit changes them, so cdhashes and the
// seals of enclosing bundles still hold.
func (h hierarchy) resign(t *testing.T, file string, edit func(cd []byte), timestamp func(signed []byte) []byte) {
	t.Helper()
	data := readTestFile(t, file)
	slices := [][]byte{data}
	if binary.BigEndian.Uint32(data) == 0xcafebabe {
		slices = nil
		for i := range int(binary.BigEndian.Uint32(data[4:8])) {
			entry := data[8+i*20:]
			offset := binary.BigEndian.Uint32(entry[8:12])
			slices = append(slices, data[offset:offset+binary.BigEndian.Uint32(entry[12:16])])
		}
	}
	for _, slice := range slices {
		sig, err := machoSlice{r: bytes.NewReader(slice), size: int64(len(slice))}.signature()
		if err != nil || len(sig.directories) != 1 {
			t.Fatalf("%s: one CodeDirectory is required: %v", file, err)
		}
		embedded := slice[sig.codeSize:]
		at := bytes.Index(embedded, sig.directories[0])
		cd := embedded[at : at+len(sig.directories[0])]
		if edit != nil {
			edit(cd)
		}
		identity := &pkgsign.Identity{Cert: h.leaf, Key: h.key, Chain: []*x509.Certificate{h.authority}}
		options := pkgsign.CMSOptions{SigningTime: h.leaf.NotBefore.Add(time.Hour)}
		der, err := pkgsign.SignCMS(cd, identity, options)
		if err != nil {
			t.Fatal(err)
		}
		if timestamp != nil {
			// The same content, key and time yield the same signature value,
			// which the token attests to.
			unstamped, _, err := pkgsign.ParseCMS(der)
			if err != nil {
				t.Fatal(err)
			}
			options.TimestampToken = timestamp(unstamped.SignatureValue)
			if der, err = pkgsign.SignCMS(cd, identity, options); err != nil {
				t.Fatal(err)
			}
		}
		previous := sig.blobs[0x10000]
		blob := cmsBlob(der)
		if len(blob) > len(previous) {
			t.Fatalf("%s: a %d-byte signature does not fit the %d bytes of the one it replaces", file, len(blob), len(previous))
		}
		at = bytes.Index(embedded, previous)
		clear(embedded[at : at+len(previous)])
		copy(embedded[at:], blob)
	}
	writeTestFile(t, file, data, 0o755)
}

// resignBundle signs every Mach-O in a bundle with the hierarchy.
func (h hierarchy) resignBundle(t *testing.T, bundle string) {
	t.Helper()
	err := filepath.WalkDir(bundle, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		data := readTestFile(t, name)
		if machO, err := isMachO(bytes.NewReader(data), int64(len(data))); err != nil || !machO {
			return err
		}
		h.resign(t, name, nil, nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (h hierarchy) verifyApp(t *testing.T, app string) (signature.Result, error) {
	t.Helper()
	root, err := os.OpenRoot(app)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	v := &bundleVerifier{ctx: t.Context(), buffer: make([]byte, 64<<10), roots: h.roots}
	return v.verifyApp(rootFS(root), filepath.Base(app), signature.Signer{})
}

func TestSigningClassNamesThePublisher(t *testing.T) {
	year := 365 * 24 * time.Hour
	expired, current := time.Now().Add(-year), time.Now().Add(year)
	store := pkix.Name{CommonName: "Apple Mac OS Application Signing", Organization: []string{"Apple Inc."}}
	developer := func(team string) pkix.Name {
		return pkix.Name{CommonName: "Developer ID Application: Example (" + team + ")", Organization: []string{"Example"}, OrganizationalUnit: []string{team}}
	}
	for _, test := range []struct {
		name            string
		authorityMarker asn1.ObjectIdentifier
		purpose         asn1.ObjectIdentifier
		subject         pkix.Name
		notAfter        time.Time
		edit            func(cd []byte)
		stamped         time.Time // when the timestamp authority dates the signature
		token           []byte    // a token in place of the authority's
		want            signature.Result
		problem         string
		unsupported     bool
	}{
		{name: "App Store", purpose: oidAppStoreApplication, subject: store, notAfter: current,
			want: signature.Result{Signer: "apple:app-store:SMLKBTR495", Authority: "Mac App Store"}},
		{name: "App Store certificate since expired", purpose: oidAppStoreApplication, subject: store, notAfter: expired,
			want: signature.Result{Signer: "apple:app-store:SMLKBTR495", Authority: "Mac App Store"}},
		{name: "App Store code naming no team", purpose: oidAppStoreApplication, subject: store, notAfter: current,
			edit: func(cd []byte) { clear(cd[48:52]) }, problem: "names no team", unsupported: true},
		{name: "Developer ID", authorityMarker: pkgsign.OIDDeveloperIDCA, purpose: pkgsign.OIDDeveloperIDApplication, subject: developer("SMLKBTR495"), notAfter: expired,
			want: signature.Result{Signer: "apple:developer-id:SMLKBTR495", Name: "Example", Authority: "Developer ID Application"}},
		{name: "Developer ID timestamped before its certificate expired", authorityMarker: pkgsign.OIDDeveloperIDCA, purpose: pkgsign.OIDDeveloperIDApplication, subject: developer("SMLKBTR495"), notAfter: expired,
			stamped: expired.Add(-time.Hour), want: signature.Result{Signer: "apple:developer-id:SMLKBTR495", Name: "Example", Authority: "Developer ID Application", Timestamped: true}},
		{name: "Developer ID timestamped after its certificate expired", authorityMarker: pkgsign.OIDDeveloperIDCA, purpose: pkgsign.OIDDeveloperIDApplication, subject: developer("SMLKBTR495"), notAfter: expired,
			stamped: expired.Add(time.Hour), problem: "certificate has expired"},
		{name: "Developer ID of another team", authorityMarker: pkgsign.OIDDeveloperIDCA, purpose: pkgsign.OIDDeveloperIDApplication, subject: developer("ABCDE12345"), notAfter: current,
			problem: "code names team SMLKBTR495 but is signed by team ABCDE12345"},
		{name: "Developer ID certificate of another authority", purpose: pkgsign.OIDDeveloperIDApplication, subject: developer("SMLKBTR495"), notAfter: current,
			problem: "neither a Developer ID Application nor an App Store certificate", unsupported: true},
		{name: "development certificate", subject: developer("SMLKBTR495"), notAfter: current,
			problem: "neither a Developer ID Application nor an App Store certificate", unsupported: true},
		{name: "timestamp that does not verify", purpose: oidAppStoreApplication, subject: store, notAfter: current,
			token: []byte{0x30, 0}, problem: "timestamp could not be verified"},
		{name: "code that expires with its certificate", purpose: oidAppStoreApplication, subject: store, notAfter: current,
			edit: func(cd []byte) { cd[14] |= 0x04 }, problem: "expires with its certificate", unsupported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := issue(t, test.authorityMarker, test.purpose, test.subject, test.notAfter)
			app := copyFixture(t, "SignedFixture.app")
			var timestamp func([]byte) []byte
			switch {
			case test.token != nil:
				timestamp = func([]byte) []byte { return test.token }
			case !test.stamped.IsZero():
				timestamp = func(signed []byte) []byte { return h.stamp(t, signed, test.stamped) }
			}
			h.resign(t, filepath.Join(app, "Contents/MacOS/fixture"), test.edit, timestamp)
			result, err := h.verifyApp(t, app)
			if test.problem != "" {
				if err == nil || !strings.Contains(err.Error(), test.problem) || errors.Is(err, ErrUnsupported) != test.unsupported {
					t.Fatalf("want %q (unsupported %v), got %+v: %v", test.problem, test.unsupported, result, err)
				}
				return
			}
			if err != nil || result.Signer != test.want.Signer || result.Name != test.want.Name || result.Authority != test.want.Authority || result.Timestamped != test.want.Timestamped {
				t.Fatalf("want %+v, got %+v: %v", test.want, result, err)
			}
		})
	}
}

// Each architecture carries its own signature, so one timestamp does not date
// the others.
func TestTimestampMustDateEveryArchitecture(t *testing.T) {
	h := issue(t, pkgsign.OIDDeveloperIDCA, pkgsign.OIDDeveloperIDApplication, pkix.Name{CommonName: "Developer ID Application: Example (SMLKBTR495)", Organization: []string{"Example"}, OrganizationalUnit: []string{"SMLKBTR495"}}, time.Now().Add(365*24*time.Hour))
	app := copyFixture(t, "SignedFixture.app")
	architectures := 0
	h.resign(t, filepath.Join(app, "Contents/MacOS/fixture"), nil, func(signed []byte) []byte {
		if architectures++; architectures > 1 {
			return nil
		}
		return h.stamp(t, signed, time.Now().Add(-time.Hour))
	})
	result, err := h.verifyApp(t, app)
	if err != nil || architectures != 2 || result.Timestamped {
		t.Fatalf("%d architectures: %+v: %v", architectures, result, err)
	}
}

// Without a timestamp macOS holds no certificate of a chain to its validity
// period, and a timestamp holds every one of them to its time. Nothing else
// about a chain is waived.
func TestValidityPeriodsApplyOnlyAtATimestamp(t *testing.T) {
	year := 365 * 24 * time.Hour
	now := time.Now()
	authorityTemplate := func(name string, from, until time.Time) *x509.Certificate {
		return &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: from, NotAfter: until, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	}
	signer := func(usage x509.ExtKeyUsage, issuer *x509.Certificate) *x509.Certificate {
		return certify(t, &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "Test Signer"}, NotBefore: now.Add(-5 * year), NotAfter: now.Add(-3 * year), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}, issuer)
	}
	// No two of these were ever valid together: the root expired before the
	// signing certificate was issued, and the authority was reissued after it
	// expired.
	rootTemplate := authorityTemplate("Test Root", now.Add(-20*year), now.Add(-10*year))
	root := certify(t, rootTemplate, rootTemplate)
	authority := certify(t, authorityTemplate("Test Authority", now.Add(-year), now.Add(year)), root)
	leaf := signer(x509.ExtKeyUsageCodeSigning, authority)
	roots := []*x509.Certificate{root}
	if _, err := appleChains(leaf, []*x509.Certificate{authority}, roots, time.Time{}, false); err != nil {
		t.Fatalf("a chain without a timestamp was held to a validity period: %v", err)
	}
	for valid, at := range map[string]time.Time{"root": now.Add(-15 * year), "signing certificate": now.Add(-4 * year), "authority": now} {
		if _, err := appleChains(leaf, []*x509.Certificate{authority}, roots, at, true); err == nil || !strings.Contains(err.Error(), "expired or is not yet valid") {
			t.Fatalf("a timestamp at which only the %s was valid was accepted: %v", valid, err)
		}
	}
	otherTemplate := authorityTemplate("Other Root", now.Add(-year), now.Add(year))
	entity := certify(t, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Test Entity"}, NotBefore: now.Add(-year), NotAfter: now.Add(year), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature}, root)
	for name, test := range map[string]struct {
		leaf                *x509.Certificate
		certificates, roots []*x509.Certificate
	}{
		"another purpose":                {signer(x509.ExtKeyUsageTimeStamping, authority), []*x509.Certificate{authority}, roots},
		"an issuer that is no authority": {signer(x509.ExtKeyUsageCodeSigning, entity), []*x509.Certificate{entity}, roots},
		"another root":                   {leaf, []*x509.Certificate{authority}, []*x509.Certificate{certify(t, otherTemplate, otherTemplate)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := appleChains(test.leaf, test.certificates, test.roots, time.Time{}, false); err == nil {
				t.Fatal("a chain without a timestamp was accepted")
			}
		})
	}
}

// The App Store signs nested code with a certificate that names no team, and
// adds its receipt to the bundle after signing.
func TestAppStoreBundle(t *testing.T) {
	h := issue(t, nil, oidAppStoreApplication, pkix.Name{CommonName: "Apple Mac OS Application Signing"}, time.Now().Add(-time.Hour))
	app := copyFixture(t, "NestedFixture.app")
	h.resignBundle(t, app)
	receipt := filepath.Join(app, "Contents/_MASReceipt/receipt")
	if err := os.Mkdir(filepath.Dir(receipt), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, receipt, []byte("receipt"), 0o644)
	result, err := h.verifyApp(t, app)
	if err != nil || result.Signer != "apple:app-store:SMLKBTR495" {
		t.Fatalf("App Store bundle: %+v: %v", result, err)
	}
	// Only the bundle's own receipt directory lies outside its envelope.
	misplaced := filepath.Join(app, "Contents/Resources/_MASReceipt/receipt")
	if err := os.Mkdir(filepath.Dir(misplaced), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, misplaced, []byte("receipt"), 0o644)
	if _, err := h.verifyApp(t, app); err == nil || !strings.Contains(err.Error(), "unsealed resource") {
		t.Fatalf("a receipt outside the receipt directory was accepted: %v", err)
	}
}

func TestCodeDirectoriesMustAgree(t *testing.T) {
	cd := signedFixtureSignature(t, 0).directories[0]
	claimed, err := codeClaims([][]byte{cd, bytes.Clone(cd)})
	if err != nil || claimed.identifier != "au.edu.vic.woodleigh.stemma.fixture" || claimed.team != "SMLKBTR495" || claimed.expires {
		t.Fatalf("fixture claims: %+v: %v", claimed, err)
	}
	for name, offset := range map[string]uint32{"identifier": binary.BigEndian.Uint32(cd[20:24]), "team": binary.BigEndian.Uint32(cd[48:52])} {
		other := bytes.Clone(cd)
		other[offset] ^= 0x01
		if _, err := codeClaims([][]byte{cd, other}); err == nil {
			t.Fatalf("CodeDirectories naming different %ss were accepted", name)
		}
	}
}
