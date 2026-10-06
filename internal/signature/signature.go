// Package signature defines the publisher signature policy shared by resource kinds.
package signature

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

// Verifier identifies the implementation whose supported subset produced a Result.
const Verifier = "stemma.signature/4"

// Schemes name the platform implementation that interprets a signer value.
const (
	AppleDeveloperID = "apple:developer-id"
	AppleAppStore    = "apple:app-store"
	Authenticode     = "authenticode"
)

// ErrMismatch reports a valid signature from a signer other than the expected one.
var ErrMismatch = errors.New("signature: unexpected signer")

// ErrUnsigned reports the absence of a signature in a supported, well-formed
// artifact. Invalid, partial and unsupported signatures must not wrap it.
var ErrUnsigned = errors.New("signature: not signed")

// BuildEvidence is the artifact evidence in which an operation that builds an
// artifact states the signing state it gave it. A builder that never signs
// records "unsigned".
const BuildEvidence = "build.signature"

// BuiltUnsigned reports whether the operation that produced artifact built it
// and left it unsigned. Its signing state is then the builder's own, with no
// publisher to expect: derivation reports nothing for it, and a declared
// expectation still verifies it.
func BuiltUnsigned(artifact plugin.Artifact) bool {
	var state string
	return json.Unmarshal(artifact.Evidence[BuildEvidence], &state) == nil && state == "unsigned"
}

// Expectation asserts the signing state of one physical subject.
type Expectation struct {
	Subject  plugin.SubjectSelector `json:"subject,omitzero" yaml:"subject,omitempty" jsonschema_description:"One physical signing subject. Omit it where there is only one, such as the PKG itself or the only application in an image. Component receipts do not carry the outer PKG signature."`
	Signer   string                 `json:"signer,omitempty" yaml:"signer,omitempty" jsonschema:"minLength=1" jsonschema_description:"Expected publisher: apple:developer-id:<TEAMID>, apple:app-store:<TEAMID> or authenticode:<SHA256>. Mutually exclusive with unsigned."`
	Unsigned bool                   `json:"unsigned,omitempty" yaml:"unsigned,omitempty" jsonschema_description:"Assert that the subject has no signature. A signed, invalid or unsupported signature fails this assertion."`
}

// JSONSchemaExtend requires exactly one assertion, with unsigned always true,
// and a selector in a subject that is declared.
func (Expectation) JSONSchemaExtend(s *jsonschema.Schema) {
	s.OneOf = []*jsonschema.Schema{{Required: []string{"signer"}}, {Required: []string{"unsigned"}}}
	field, _ := s.Properties.Get("unsigned")
	field.Enum = []any{true}
	subject, _ := s.Properties.Get("subject")
	subject.MinProperties = new(uint64(1))
}

// Validate checks a declaration. A signer uses one of the schemes the kind
// verifies.
func (e Expectation) Validate(schemes ...string) error {
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
	if !slices.Contains(schemes, signer.Scheme) {
		return fmt.Errorf("signer must use %s", strings.Join(schemes, " or "))
	}
	return nil
}

// InputExpectation applies the same assertion to a consumed builder input.
type InputExpectation struct {
	Expectation `yaml:",inline"`

	Input string `json:"input" yaml:"input" jsonschema:"minLength=1" jsonschema_description:"Declared input whose selected content the build consumes."`
}

// Signer is a canonical publisher identity that survives certificate renewal.
// Apple software uses the publisher's team, under the scheme of the certificate
// that signed it: the publisher's own Developer ID, or Apple's for the App
// Store. Windows software uses a digest of the publisher and its issuing
// authority.
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
	case AppleDeveloperID, AppleAppStore:
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
	Signer    string `json:"signer,omitempty" jsonschema_description:"Expected publisher identity: apple:developer-id:<TEAMID>, apple:app-store:<TEAMID> or authenticode:<SHA256>. Use stemma signature to inspect the input and derive it."`
	Name      string `json:"name,omitempty"`
	Authority string `json:"authority,omitempty"`
	Target    string `json:"target,omitempty"`
	Verifier  string `json:"verifier"`
	// Timestamped reports that a timestamp authority under the verifier's
	// trust anchors dated the signature, so its certificates were judged at
	// that time. Only the Apple verifiers trust one.
	Timestamped bool `json:"timestamped,omitempty"`
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
			subject, err := selectSubject(subjects, expected.Subject)
			if err != nil {
				paths := make([]string, len(subjects))
				for i, candidate := range subjects {
					paths[i] = candidate.Path
				}
				return nil, fmt.Errorf("signature subject: %w; signing subjects: %s", err, strings.Join(paths, ", "))
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

// selectSubject resolves a declaration within a signing scope. One that names
// no subject covers the scope's only subject.
func selectSubject(subjects []plugin.Subject, selector plugin.SubjectSelector) (plugin.Subject, error) {
	if selector != (plugin.SubjectSelector{}) {
		return plugin.SelectSubject(plugin.Facts{Subjects: subjects}, selector)
	}
	if len(subjects) != 1 {
		return plugin.Subject{}, errors.New("an entry without subject needs exactly one signing subject")
	}
	return subjects[0], nil
}

// Notices returns the advisories that observations raise. They describe
// evidence a valid signature lacks and never fail a resource.
func Notices(observations []Observation) []plugin.Notice {
	// The published artifact is one signing scope and each build input another.
	scope := map[string]int{}
	for _, observed := range observations {
		scope[observed.Input]++
	}
	var notices []plugin.Notice
	for _, observed := range observations {
		// macOS accepts Developer ID code that no timestamp dates, without
		// holding it to its certificate's validity period, and so does the
		// verifier. A package is instead held to it when verified. Evidence
		// from another verifier does not say whether a timestamp exists.
		if observed.State != "signed" || observed.Verifier != Verifier || observed.Timestamped ||
			observed.Authority != "Developer ID Application" || !strings.HasPrefix(observed.Signer, AppleDeveloperID+":") {
			continue
		}
		var where []string
		if observed.Input != "" {
			where = append(where, "Input: "+observed.Input)
		}
		if scope[observed.Input] > 1 {
			where = append(where, "Subject: "+observed.Subject.Path)
		}
		notices = append(notices, plugin.Notice{
			Level: "warning", Code: "signature-timestamp-missing",
			Message: "Developer ID signature has no secure timestamp; certificate validity at signing time cannot be established",
			Hint:    strings.Join(where, ", "),
		})
	}
	return notices
}

// Fragment renders all observations as a complete declaration for review.
func Fragment(observations []Observation) string {
	if len(observations) == 0 {
		return ""
	}
	// The published artifact is one signing scope and each build input another.
	scope := map[string]int{}
	for _, observed := range observations {
		scope[observed.Input]++
	}
	var text strings.Builder
	text.WriteString("signatures:\n")
	for _, observed := range observations {
		text.WriteString("  - ")
		if observed.Input != "" {
			fmt.Fprintf(&text, "input: %s\n    ", strconv.Quote(observed.Input))
		}
		// A declaration that names no subject covers the only subject of its
		// scope.
		if scope[observed.Input] > 1 {
			fmt.Fprintf(&text, "subject:\n      path: %s\n    ", strconv.Quote(observed.Subject.Path))
		}
		if observed.State == "unsigned" {
			text.WriteString("unsigned: true\n")
			continue
		}
		fmt.Fprintf(&text, "signer: %s", observed.Signer)
		if observed.Name != "" {
			fmt.Fprintf(&text, " # %s", strings.NewReplacer("\n", " ", "\r", " ").Replace(observed.Name))
		}
		text.WriteByte('\n')
	}
	return text.String()
}
