// Package apple inspects macOS artifacts and verifies their code signatures.
package apple

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/deploymenttheory/go-macos-pkg/pkg/pkgsign"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/signature"
)

// ErrUnsupported means an artifact exceeds the supported signing subset.
var ErrUnsupported = errors.New("unsupported Apple artifact verification")

// Apple marks certificate purposes with critical extensions that Go cannot
// interpret; they are checked here instead.
var oidAppleExtensions = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6}

// oidAppStoreApplication marks the certificate Apple signs App Store
// applications with.
var oidAppStoreApplication = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 1, 9}

// signingClass is the purpose of the Apple certificate that signed code.
// Gatekeeper tells the classes apart by the same certificate markers.
type signingClass int

const (
	// classOther is a code signing certificate under Apple's roots that names
	// no publisher Stemma expects. It authenticates nested code only.
	classOther signingClass = iota
	// classDeveloperID is a Developer ID Application certificate issued by a
	// Developer ID Certification Authority: the publisher signed the code.
	classDeveloperID
	// classAppStore is Apple's own certificate for App Store applications:
	// Apple signed the code a publisher submitted.
	classAppStore
)

// codeIdentity describes one authenticated signer of a code item.
type codeIdentity struct {
	identifier string
	class      signingClass
	// team is the signer's team: the signing certificate's, except under the
	// App Store, where the certificate is Apple's and the CodeDirectory names
	// the publisher's team.
	team string
	// name is the organisation in the signing certificate. App Store code has
	// none, since its certificate names Apple.
	name string
	// cdhashes lists every CodeDirectory hash across architectures.
	cdhashes [][]byte
	// timestamped reports that a trusted timestamp dates the signature of
	// every architecture.
	timestamped bool
}

func (c codeIdentity) same(other codeIdentity) bool {
	return c.identifier == other.identifier && c.class == other.class && c.team == other.team && c.name == other.name
}

// publisher returns the signer code was signed for and the authority that
// vouches for it, or the zero signer when its class names no publisher.
func (c codeIdentity) publisher() (signature.Signer, string) {
	switch c.class {
	case classDeveloperID:
		return signature.Signer{Scheme: signature.AppleDeveloperID, Value: c.team}, "Developer ID Application"
	case classAppStore:
		return signature.Signer{Scheme: signature.AppleAppStore, Value: c.team}, "Mac App Store"
	case classOther:
	}
	return signature.Signer{}, ""
}

// certificateTime returns the time at which a signature's certificates must
// have been valid, which only a timestamp token fixes. The token must itself
// verify under the same roots. timestamped is false for a signature without
// one, and macOS then holds none of its certificates to a validity period: App
// Store signatures carry no timestamp, and Apple's certificate for them expires
// while the applications it signed stay installed.
func certificateTime(cms *cmsSignature, expires bool, roots []*x509.Certificate) (at time.Time, timestamped bool, err error) {
	if len(cms.timestampToken) != 0 {
		authorities := x509.NewCertPool()
		for _, root := range roots {
			authorities.AddCert(root)
		}
		at, err = pkgsign.VerifyTimestamp(cms.timestampToken, cms.signatureValue, authorities)
		return at, err == nil, err
	}
	if expires {
		// macOS judges such code at the time its signer states, or now.
		return time.Time{}, false, fmt.Errorf("%w: code that expires with its certificate has no trusted timestamp", ErrUnsupported)
	}
	return time.Time{}, false, nil
}

// appleChains builds the chains from a signing certificate to the roots for
// code signing. With a timestamp, every certificate of a chain must have been
// valid at its time. Without one no validity period applies, as on macOS. A
// chain's signatures, constraints and purpose are checked either way.
func appleChains(leaf *x509.Certificate, certificates, roots []*x509.Certificate, at time.Time, timestamped bool) ([][]*x509.Certificate, error) {
	if !timestamped {
		// Any time serves, since no certificate is held to one.
		at = time.Unix(0, 0)
	}
	// Verify cannot skip validity periods, and its error for one comes before
	// the checks that follow. Without a timestamp it is given copies valid at
	// the time it is asked about. A signature covers the certificate as issued,
	// so a copy differs in nothing else.
	offered := func(certificate *x509.Certificate) *x509.Certificate {
		if timestamped {
			return certificate
		}
		undated := *certificate
		undated.NotBefore, undated.NotAfter = at, at
		return &undated
	}
	options := x509.VerifyOptions{Roots: x509.NewCertPool(), Intermediates: x509.NewCertPool(), CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	for _, root := range roots {
		options.Roots.AddCert(offered(root))
	}
	for _, certificate := range certificates {
		if !certificate.Equal(leaf) {
			acceptAppleExtensions(certificate)
			options.Intermediates.AddCert(offered(certificate))
		}
	}
	acceptAppleExtensions(leaf)
	chains, err := offered(leaf).Verify(options)
	if err != nil {
		return nil, fmt.Errorf("signing certificate is not trusted by Apple's roots: %w", err)
	}
	return chains, nil
}

// classify reads the signing class from verified chains. Requirements name
// certificate 1 of the verified chain, so an authority marker elsewhere among
// a signature's certificates does not count.
func classify(chains [][]*x509.Certificate) signingClass {
	leaf := chains[0][0]
	if hasExtension(leaf, pkgsign.OIDDeveloperIDApplication) {
		for _, chain := range chains {
			if len(chain) > 1 && hasExtension(chain[1], pkgsign.OIDDeveloperIDCA) {
				return classDeveloperID
			}
		}
	}
	if hasExtension(leaf, oidAppStoreApplication) {
		return classAppStore
	}
	return classOther
}

// identify names the signer verified chains stand for. directoryTeam is the
// team the signed CodeDirectory names, if any.
func identify(chains [][]*x509.Certificate, directoryTeam string) (codeIdentity, error) {
	leaf := chains[0][0]
	identity := codeIdentity{class: classify(chains)}
	if identity.class == classAppStore {
		// Apple signs the CodeDirectory of the code a team submitted, and that
		// CodeDirectory names the team.
		if directoryTeam == "" {
			return codeIdentity{}, fmt.Errorf("%w: App Store signature names no team", ErrUnsupported)
		}
		identity.team = directoryTeam
		return identity, nil
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] == "" {
		return codeIdentity{}, fmt.Errorf("%w: signing certificate has no team", ErrUnsupported)
	}
	identity.team = leaf.Subject.OrganizationalUnit[0]
	if len(leaf.Subject.Organization) > 0 {
		identity.name = leaf.Subject.Organization[0]
	}
	// macOS rejects a developer's signature over code that names another team.
	if directoryTeam != "" && directoryTeam != identity.team {
		return codeIdentity{}, fmt.Errorf("code names team %s but is signed by team %s", directoryTeam, identity.team)
	}
	return identity, nil
}

func hasExtension(certificate *x509.Certificate, id asn1.ObjectIdentifier) bool {
	return slices.ContainsFunc(certificate.Extensions, func(extension pkix.Extension) bool { return extension.Id.Equal(id) })
}

func acceptAppleExtensions(certificate *x509.Certificate) {
	certificate.UnhandledCriticalExtensions = slices.DeleteFunc(certificate.UnhandledCriticalExtensions, func(id asn1.ObjectIdentifier) bool {
		return len(id) > len(oidAppleExtensions) && id[:len(oidAppleExtensions)].Equal(oidAppleExtensions)
	})
}

func matchCDHash(identity codeIdentity, sealed []byte) bool {
	for _, cdhash := range identity.cdhashes {
		if slices.Equal(cdhash, sealed) {
			return true
		}
	}
	return false
}

func fileDigest(ctx context.Context, f io.Reader, buffer []byte) (string, error) {
	h := sha256.New()
	if _, err := io.CopyBuffer(h, fileio.Reader{Context: ctx, Reader: f}, buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
