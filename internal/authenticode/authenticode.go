// Package authenticode verifies Windows installer signatures against an
// expected publisher.
package authenticode

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sassoftware/relic/v8/lib/authenticode"
	"github.com/sassoftware/relic/v8/lib/pkcs9"
	"github.com/sassoftware/relic/v8/signers/sigerrors"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/signature"
)

// ErrUnsupported means a file or signature exceeds the supported subset.
var ErrUnsupported = errors.New("unsupported Authenticode verification")

const identityVersion = "stemma.authenticode/1"

type publisher struct {
	signer    signature.Signer
	name      string
	authority string
}

// Verify checks that every Authenticode signature covers the file's signed
// content and that all of them belong to one publisher, then reports it. A
// zero want derives the signer; otherwise it must match.
//
// The publisher identity digests the certificate subject together with the
// issuing authority's public key, so it survives routine renewal while a
// change of authority or publisher becomes visible. That authority is also
// the trust anchor; no operating-system root store is consulted. A timestamp
// countersignature fixes the time at which certificate validity is judged.
func Verify(ctx context.Context, filePath string, want signature.Signer) (signature.Result, error) {
	if err := ctx.Err(); err != nil {
		return signature.Result{}, err
	}
	kind := strings.ToLower(filepath.Ext(filePath))
	if kind != ".msi" && kind != ".exe" && kind != ".dll" {
		return signature.Result{}, fmt.Errorf("%w: %s files", ErrUnsupported, filepath.Ext(filePath))
	}
	f, err := os.Open(filePath)
	if err != nil {
		return signature.Result{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return signature.Result{}, err
	}
	if !info.Mode().IsRegular() {
		return signature.Result{}, errors.New("installer must be a regular file")
	}
	var signatures []pkcs9.TimestampedSignature
	if kind == ".msi" {
		signed, err := authenticode.VerifyMSI(contextReaderAt{ctx, f}, false)
		if err != nil {
			return signature.Result{}, classify(err)
		}
		signatures = append(signatures, signed.TimestampedSignature)
	} else {
		signed, err := authenticode.VerifyPE(&contextReadSeeker{ctx, f}, false)
		if err != nil {
			return signature.Result{}, classify(err)
		}
		for _, entry := range signed {
			signatures = append(signatures, entry.TimestampedSignature)
		}
	}
	if len(signatures) == 0 {
		return signature.Result{}, errors.New("installer is not signed")
	}
	var identity publisher
	for i, signed := range signatures {
		current, err := identify(signed)
		if err != nil {
			return signature.Result{}, err
		}
		if i == 0 {
			identity = current
		} else if current.signer != identity.signer {
			return signature.Result{}, fmt.Errorf("%w: signatures by different publishers", ErrUnsupported)
		}
	}
	result := signature.Result{Signer: identity.signer.String(), Name: identity.name, Authority: identity.authority, Target: filepath.Base(filePath), Verifier: signature.Verifier}
	if err := signature.Check(identity.signer, want); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

func classify(err error) error {
	if _, unsigned := errors.AsType[sigerrors.NotSignedError](err); unsigned {
		return errors.New("installer is not signed")
	}
	return fmt.Errorf("installer signature: %w", err)
}

func identify(signed pkcs9.TimestampedSignature) (publisher, error) {
	leaf := signed.Certificate
	if leaf == nil {
		return publisher{}, errors.New("signature has no signer certificate")
	}
	var issuer *x509.Certificate
	for _, candidate := range signed.Intermediates {
		if candidate.IsCA && leaf.CheckSignatureFrom(candidate) == nil {
			issuer = candidate
			break
		}
	}
	if issuer == nil {
		return publisher{}, fmt.Errorf("%w: signature does not include the issuing authority certificate", ErrUnsupported)
	}
	var at time.Time
	if signed.CounterSignature != nil {
		at = signed.CounterSignature.SigningTime
	}
	anchors := x509.NewCertPool()
	anchors.AddCert(issuer)
	if err := signed.Signature.VerifyChain(anchors, nil, x509.ExtKeyUsageCodeSigning, at); err != nil {
		return publisher{}, fmt.Errorf("signing certificate: %w", err)
	}
	key := sha256.Sum256(issuer.RawSubjectPublicKeyInfo)
	digest := sha256.Sum256([]byte(identityVersion + "\n" + leaf.Subject.String() + "\n" + hex.EncodeToString(key[:])))
	name := leaf.Subject.CommonName
	if name == "" && len(leaf.Subject.Organization) > 0 {
		name = leaf.Subject.Organization[0]
	}
	return publisher{signer: signature.Signer{Scheme: signature.Authenticode, Value: hex.EncodeToString(digest[:])}, name: name, authority: issuer.Subject.CommonName}, nil
}

type contextReaderAt struct {
	context.Context
	io.ReaderAt
}

func (r contextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(p, off)
}

type contextReadSeeker struct {
	ctx context.Context
	f   *os.File
}

func (r *contextReadSeeker) Read(p []byte) (int, error) {
	return fileio.Reader{Context: r.ctx, Reader: r.f}.Read(p)
}

func (r *contextReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.f.Seek(offset, whence)
}
