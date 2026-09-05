// Package apple inspects macOS artifacts and verifies explicitly bounded signature scopes.
package apple

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Verifier identifies the implementation whose supported subset produced evidence.
const Verifier = "stemma.apple/0.1.0"

// Verification result states are distinct from absence of a verification request.
const (
	Valid        = "valid"
	Invalid      = "invalid"
	Unsupported  = "unsupported"
	NotRequested = "not_requested"
)

// ErrUnsupported means an artifact or requested check exceeds the supported subset.
var ErrUnsupported = errors.New("unsupported Apple artifact verification")

// Policy declares independent required checks. CertificateSHA256 is an exact DER
// certificate pin; it neither implies certificate-chain trust nor a time policy.
type Policy struct {
	RequireIntegrity  bool   `json:"require_integrity"`
	RequireSignature  bool   `json:"require_signature"`
	RequireResources  bool   `json:"require_resources"`
	RequireIdentity   bool   `json:"require_identity"`
	CertificateSHA256 string `json:"certificate_sha256,omitempty"`
	RequirePlatform   bool   `json:"require_platform"`
}

func (p Policy) expanded() Policy {
	if p.CertificateSHA256 != "" {
		p.RequireIdentity = true
	}
	if p.RequireIdentity {
		p.RequireSignature = true
	}
	if p.RequireSignature || p.RequireResources {
		p.RequireIntegrity = true
	}
	return p
}

// Check records one check without promoting inspection to verification.
type Check struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Evidence binds verification results to exact bytes, implementation and policy.
// For apps, SubjectSHA256 identifies the complete regular-file tree (see VerifyApp).
type Evidence struct {
	SubjectSHA256 string `json:"subject_sha256"`
	Verifier      string `json:"verifier"`
	PolicySHA256  string `json:"policy_sha256"`
	Integrity     Check  `json:"integrity"`
	Signature     Check  `json:"signature"`
	Resources     Check  `json:"resources"`
	Identity      Check  `json:"identity"`
	Platform      Check  `json:"platform"`
}

func newEvidence(digest string, policy Policy) Evidence {
	data, _ := json.Marshal(policy)
	policyDigest := sha256.Sum256(data)
	return Evidence{
		SubjectSHA256: digest, Verifier: Verifier, PolicySHA256: hex.EncodeToString(policyDigest[:]),
		Integrity: Check{Status: NotRequested}, Signature: Check{Status: NotRequested},
		Resources: Check{Status: NotRequested}, Identity: Check{Status: NotRequested},
		Platform: Check{Status: NotRequested},
	}
}

func (e Evidence) required(policy Policy) error {
	checks := []struct {
		name     string
		required bool
		check    Check
	}{
		{"integrity", policy.RequireIntegrity, e.Integrity},
		{"signature", policy.RequireSignature, e.Signature},
		{"resources", policy.RequireResources, e.Resources},
		{"identity", policy.RequireIdentity || policy.CertificateSHA256 != "", e.Identity},
		{"platform", policy.RequirePlatform, e.Platform},
	}
	var failures []error
	for _, c := range checks {
		if !c.required || c.check.Status == Valid {
			continue
		}
		if c.check.Status == Unsupported {
			failures = append(failures, fmt.Errorf("%s: %w: %s", c.name, ErrUnsupported, c.check.Detail))
		} else {
			failures = append(failures, fmt.Errorf("%s %s: %s", c.name, c.check.Status, c.check.Detail))
		}
	}
	return errors.Join(failures...)
}

func checkError(err error, detail string) Check {
	if err == nil {
		return Check{Status: Valid, Detail: detail}
	}
	status := Invalid
	if errors.Is(err, ErrUnsupported) {
		status = Unsupported
	}
	return Check{Status: status, Detail: err.Error()}
}

func fileDigest(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
