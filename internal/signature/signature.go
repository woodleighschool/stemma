// Package signature defines the publisher signature policy shared by resource kinds.
package signature

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Verifier identifies the implementation whose supported subset produced a Result.
const Verifier = "stemma.signature/1"

// Schemes name the platform implementation that interprets a signer value.
const (
	AppleDeveloperID = "apple:developer-id"
	Authenticode     = "authenticode"
)

// ErrMismatch reports a valid signature from a signer other than the expected one.
var ErrMismatch = errors.New("signature: unexpected signer")

// Policy requires the published artifact to carry a complete, valid signature
// from one expected signer. Derive the value with `stemma signature`.
type Policy struct {
	Signer string `json:"signer" yaml:"signer" jsonschema:"required" jsonschema_description:"Expected publisher identity: apple:developer-id:<TEAMID> or authenticode:<SHA256>. Use stemma signature to inspect the input and derive it."`
}

// Signer is a canonical publisher identity that survives certificate renewal.
// Apple software uses the Developer ID team; Windows software uses a digest of
// the publisher and its issuing authority.
type Signer struct {
	Scheme string
	Value  string
}

// Parse validates a signer value. Each scheme fixes the shape of its value.
func Parse(text string) (Signer, error) {
	scheme, value, found := strings.Cut(strings.TrimSpace(text), ":")
	if found && scheme == "apple" {
		var rest string
		if rest, value, found = strings.Cut(value, ":"); found {
			scheme += ":" + rest
		}
	}
	if !found || value == "" {
		return Signer{}, fmt.Errorf("signer %q requires a scheme and value", text)
	}
	switch scheme {
	case AppleDeveloperID:
		if len(value) != 10 || strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
			return Signer{}, fmt.Errorf("signer %q requires a ten-character Apple Team ID", text)
		}
	case Authenticode:
		if digest, err := hex.DecodeString(value); err != nil || len(digest) != 32 || value != strings.ToLower(value) {
			return Signer{}, fmt.Errorf("signer %q requires a lowercase SHA-256 publisher digest", text)
		}
	default:
		return Signer{}, fmt.Errorf("signer %q uses an unsupported scheme", text)
	}
	return Signer{Scheme: scheme, Value: value}, nil
}

// IsZero reports an unset signer, which asks a verifier to derive one.
func (s Signer) IsZero() bool {
	return s.Scheme == "" && s.Value == ""
}

func (s Signer) String() string {
	if s.IsZero() {
		return ""
	}
	return s.Scheme + ":" + s.Value
}

// Result records one complete, valid signature. Name is display information
// from the signing certificate and never participates in verification.
type Result struct {
	Signer    string `json:"signer" jsonschema_description:"Expected publisher identity: apple:developer-id:<TEAMID> or authenticode:<SHA256>. Use stemma signature to inspect the input and derive it."`
	Name      string `json:"name,omitempty"`
	Authority string `json:"authority,omitempty"`
	Target    string `json:"target"`
	Verifier  string `json:"verifier"`
}

// Check compares an observed signer with the expected one, unless deriving.
func Check(observed, expected Signer) error {
	if expected.IsZero() || observed == expected {
		return nil
	}
	return fmt.Errorf("%w: expected %s, observed %s", ErrMismatch, expected, observed)
}

// Fragment renders the policy to paste into a resource document.
func (r Result) Fragment() string {
	line := "signature:\n  signer: " + r.Signer
	if r.Name != "" {
		line += " # " + strings.NewReplacer("\n", " ", "\r", " ").Replace(r.Name)
	}
	return line + "\n"
}

// InputResult records the verified signature of an input a build consumed. It
// describes that input and never the artifact built from it.
type InputResult struct {
	Result

	Input string `json:"input"`
}

// Fragment renders the input policy to paste into a builder document.
func (r InputResult) Fragment() string {
	return "signature:\n  input: " + r.Input + "\n" + strings.TrimPrefix(r.Result.Fragment(), "signature:\n")
}
