// Package signature defines the publisher signature policy shared by resource kinds.
package signature

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

// Verifier identifies the implementation whose supported subset produced a Result.
const Verifier = "stemma.signature/3"

// Schemes name the platform implementation that interprets a signer value.
const (
	AppleDeveloperID = "apple:developer-id"
	Authenticode     = "authenticode"
)

// ErrMismatch reports a valid signature from a signer other than the expected one.
var ErrMismatch = errors.New("signature: unexpected signer")

// ErrUnsigned reports the absence of a signature in a supported, well-formed
// artifact. Invalid, partial and unsupported signatures must not wrap it.
var ErrUnsigned = errors.New("signature: not signed")

// Expectation asserts the signing state of one physical subject.
type Expectation struct {
	Subject  plugin.SubjectSelector `json:"subject" yaml:"subject" jsonschema_description:"Exactly one physical signing subject. A scalar artifact uses path: .; component receipts do not carry the outer PKG signature."`
	Signer   string                 `json:"signer,omitempty" yaml:"signer,omitempty" jsonschema:"minLength=1" jsonschema_description:"Expected apple:developer-id:<TEAMID> or authenticode:<SHA256> publisher. Mutually exclusive with unsigned."`
	Unsigned bool                   `json:"unsigned,omitempty" yaml:"unsigned,omitempty" jsonschema_description:"Assert that the subject has no signature. A signed, invalid or unsupported signature fails this assertion."`
}

// JSONSchemaExtend requires exactly one assertion, with unsigned always true.
func (Expectation) JSONSchemaExtend(s *jsonschema.Schema) {
	s.OneOf = []*jsonschema.Schema{{Required: []string{"signer"}}, {Required: []string{"unsigned"}}}
	field, _ := s.Properties.Get("unsigned")
	field.Enum = []any{true}
	subject, _ := s.Properties.Get("subject")
	subject.MinProperties = new(uint64(1))
}

func (e Expectation) Validate(scheme string) error {
	if e.Subject == (plugin.SubjectSelector{}) {
		return errors.New("subject requires an explicit selector")
	}
	if p := e.Subject.Path; p != "" && (!fs.ValidPath(p) || strings.ContainsAny(p, "\\\x00\r\n\t")) {
		return errors.New("subject.path must be an exact confined path")
	}
	if (e.Signer != "") == e.Unsigned {
		return errors.New("declare exactly one of signer or unsigned: true")
	}
	if e.Unsigned {
		return nil
	}
	signer, err := Parse(e.Signer)
	if err != nil {
		return err
	}
	if signer.Scheme != scheme {
		return fmt.Errorf("signer must use %s", scheme)
	}
	return nil
}

// InputExpectation applies the same assertion to a consumed builder input.
type InputExpectation struct {
	Expectation `yaml:",inline"`

	Input string `json:"input" yaml:"input" jsonschema:"minLength=1" jsonschema_description:"Declared input whose selected content the build consumes."`
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
	Signer    string `json:"signer,omitempty" jsonschema_description:"Expected publisher identity: apple:developer-id:<TEAMID> or authenticode:<SHA256>. Use stemma signature to inspect the input and derive it."`
	Name      string `json:"name,omitempty"`
	Authority string `json:"authority,omitempty"`
	Target    string `json:"target,omitempty"`
	Verifier  string `json:"verifier"`
	// Replaced lists nested code the signature accepts through the
	// requirement it recorded, not the exact code it sealed.
	Replaced []Replacement `json:"replaced,omitempty"`
}

// Replacement is nested code that differs from the code its parent sealed and
// satisfies the requirement the parent recorded for it. Path is relative to
// the signed target.
type Replacement struct {
	Path     string   `json:"path"`
	Sealed   string   `json:"sealed_cdhash"`
	CDHashes []string `json:"cdhashes"`
}

// Check compares an observed signer with the expected one, unless deriving.
func Check(observed, expected Signer) error {
	if expected.IsZero() || observed == expected {
		return nil
	}
	return fmt.Errorf("%w: expected %s, observed %s", ErrMismatch, expected, observed)
}

// Observation describes a physical subject, independently of its expectation.
// Input is empty only when the subject belongs to the published artifact.
type Observation struct {
	Result

	Input   string                 `json:"input,omitempty"`
	Subject plugin.SubjectSelector `json:"subject"`
	State   string                 `json:"state"`
}

// Verify resolves declarations against the complete signing scope before doing
// I/O. Derivation observes every subject without enforcing declarations.
func Verify(ctx context.Context, expectations []Expectation, subjects []plugin.Subject, derive bool, check func(plugin.Subject) (Result, error)) ([]Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	policies := map[string]Expectation{}
	if !derive {
		for _, expected := range expectations {
			subject, err := plugin.SelectSubject(plugin.Facts{Subjects: subjects}, expected.Subject)
			if err != nil {
				return nil, fmt.Errorf("signature subject: %w", err)
			}
			if _, exists := policies[subject.ID]; exists {
				return nil, fmt.Errorf("duplicate signature expectation for %q", subject.Path)
			}
			if expected.Signer != "" {
				signer, err := Parse(expected.Signer)
				if err != nil {
					return nil, err
				}
				expected.Signer = signer.String()
			}
			policies[subject.ID] = expected
		}
		for _, subject := range subjects {
			if _, exists := policies[subject.ID]; !exists {
				return nil, fmt.Errorf("missing signature expectation for %q", subject.Path)
			}
		}
	}
	observations := make([]Observation, 0, len(subjects))
	for _, subject := range subjects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		done := plugin.Stage(ctx, "Inspecting signature", plugin.Detail(subject.Path))
		result, err := check(subject)
		if errors.Is(err, ErrUnsigned) {
			done(nil, plugin.Detail("unsigned"))
		} else {
			done(err, plugin.Detail(result.Name))
		}
		observed := Observation{Subject: plugin.SubjectSelector{Path: subject.Path}, State: "signed", Result: result}
		if errors.Is(err, ErrUnsigned) {
			observed.State, observed.Result = "unsigned", Result{Verifier: Verifier}
		} else if err != nil {
			return nil, fmt.Errorf("signature %q: %w", subject.Path, err)
		}
		if !derive {
			expected := policies[subject.ID]
			switch {
			case expected.Unsigned && observed.State != "unsigned":
				return nil, fmt.Errorf("signature %q: expected unsigned, observed %s", subject.Path, observed.Signer)
			case !expected.Unsigned && observed.State == "unsigned":
				return nil, fmt.Errorf("signature %q: expected %s: %w", subject.Path, expected.Signer, ErrUnsigned)
			case !expected.Unsigned && observed.Signer != expected.Signer:
				return nil, fmt.Errorf("signature %q: %w: expected %s, observed %s", subject.Path, ErrMismatch, expected.Signer, observed.Signer)
			}
		}
		observations = append(observations, observed)
	}
	return observations, nil
}

// Fragment renders all observations as a complete declaration for review.
func Fragment(observations []Observation) string {
	if len(observations) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString("signatures:\n")
	for _, observed := range observations {
		text.WriteString("  - ")
		if observed.Input != "" {
			fmt.Fprintf(&text, "input: %s\n    ", strconv.Quote(observed.Input))
		}
		fmt.Fprintf(&text, "subject:\n      path: %s\n", strconv.Quote(observed.Subject.Path))
		if observed.State == "unsigned" {
			text.WriteString("    unsigned: true\n")
			continue
		}
		fmt.Fprintf(&text, "    signer: %s", observed.Signer)
		if observed.Name != "" {
			fmt.Fprintf(&text, " # %s", strings.NewReplacer("\n", " ", "\r", " ").Replace(observed.Name))
		}
		text.WriteByte('\n')
	}
	return text.String()
}
