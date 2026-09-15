// Package apple inspects macOS artifacts and verifies their code signatures.
package apple

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
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

// codeIdentity describes one authenticated signer of a code item.
type codeIdentity struct {
	identifier  string
	teamID      string
	name        string
	application bool
	installer   bool
	// cdhashes lists every CodeDirectory hash across architectures.
	cdhashes [][]byte
}

func (c codeIdentity) same(other codeIdentity) bool {
	return c.identifier == other.identifier && c.teamID == other.teamID && c.name == other.name && c.application == other.application && c.installer == other.installer
}

func (c codeIdentity) signer() signature.Signer {
	return signature.Signer{Scheme: signature.AppleDeveloperID, Value: c.teamID}
}

// identify authenticates a signing certificate against Apple's roots at the
// signature's trusted time and reads the Developer ID team it belongs to.
func identify(leaf *x509.Certificate, certificates []*x509.Certificate, at time.Time) (codeIdentity, error) {
	intermediates := x509.NewCertPool()
	for _, certificate := range certificates {
		if !certificate.Equal(leaf) {
			acceptAppleExtensions(certificate)
			intermediates.AddCert(certificate)
		}
	}
	acceptAppleExtensions(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pkgsign.AppleRoots(), Intermediates: intermediates, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err != nil {
		return codeIdentity{}, fmt.Errorf("signing certificate is not trusted by Apple's roots: %w", err)
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] == "" {
		return codeIdentity{}, fmt.Errorf("%w: signing certificate has no team", ErrUnsupported)
	}
	identity := codeIdentity{teamID: leaf.Subject.OrganizationalUnit[0]}
	if len(leaf.Subject.Organization) > 0 {
		identity.name = leaf.Subject.Organization[0]
	}
	for _, extension := range leaf.Extensions {
		switch {
		case extension.Id.Equal(pkgsign.OIDDeveloperIDApplication):
			identity.application = true
		case extension.Id.Equal(pkgsign.OIDDeveloperIDInstaller):
			identity.installer = true
		}
	}
	return identity, nil
}

func acceptAppleExtensions(certificate *x509.Certificate) {
	certificate.UnhandledCriticalExtensions = slices.DeleteFunc(certificate.UnhandledCriticalExtensions, func(id asn1.ObjectIdentifier) bool {
		return len(id) > len(oidAppleExtensions) && id[:len(oidAppleExtensions)].Equal(oidAppleExtensions)
	})
}

// trustedTime returns the time a trusted timestamp authority saw the
// signature, or zero to judge certificates now. A token that does not attest
// to this signature is tampering.
func trustedTime(token, signatureValue []byte) (time.Time, error) {
	if len(token) == 0 {
		return time.Time{}, nil
	}
	at, err := pkgsign.VerifyTimestamp(token, signatureValue, pkgsign.AppleRoots())
	if errors.Is(err, pkgsign.ErrTimestampInvalid) {
		return time.Time{}, err
	}
	if err != nil {
		// An authority Stemma cannot check leaves certificates judged now.
		at = time.Time{}
	}
	return at, nil
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
