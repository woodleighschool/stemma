package signature

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

func TestParseAcceptsCanonicalSigners(t *testing.T) {
	for _, test := range []struct {
		text string
		want Signer
	}{
		{"apple:developer-id:UBF8T346G9", Signer{Scheme: AppleDeveloperID, Value: "UBF8T346G9"}},
		{"\tapple:developer-id:SMLKBTR495\n", Signer{Scheme: AppleDeveloperID, Value: "SMLKBTR495"}},
		{"apple:app-store:L82V4Y2P3C", Signer{Scheme: AppleAppStore, Value: "L82V4Y2P3C"}},
		{"authenticode:" + strings.Repeat("0a", 32), Signer{Scheme: Authenticode, Value: strings.Repeat("0a", 32)}},
	} {
		signer, err := Parse(test.text)
		if err != nil || signer != test.want || signer.String() != strings.TrimSpace(test.text) {
			t.Fatalf("Parse(%q) = %+v, %v; want %+v", test.text, signer, err, test.want)
		}
	}
	for _, text := range []string{"", "UBF8T346G9", "apple:UBF8T346G9", "apple:developer-id:", "apple:developer-id:ubf8t346g9", "apple:developer-id:UBF8T346G9X", "apple:app-store:", "apple:app-store:l82v4y2p3c", "apple:store:L82V4Y2P3C", "authenticode:", "authenticode:" + strings.Repeat("0A", 32), "authenticode:" + strings.Repeat("0a", 31), "codesign:UBF8T346G9"} {
		if signer, err := Parse(text); err == nil {
			t.Fatalf("Parse(%q) accepted %+v", text, signer)
		}
	}
}

func TestCheckAndFragment(t *testing.T) {
	observed := Signer{Scheme: AppleDeveloperID, Value: "UBF8T346G9"}
	if err := Check(observed, Signer{}); err != nil {
		t.Fatalf("derivation rejected: %v", err)
	}
	if err := Check(observed, observed); err != nil {
		t.Fatalf("matching signer rejected: %v", err)
	}
	err := Check(observed, Signer{Scheme: AppleDeveloperID, Value: "AAAAAAAAAA"})
	if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), "observed apple:developer-id:UBF8T346G9") {
		t.Fatalf("mismatch: %v", err)
	}
}

func TestFragmentPreservesSubjectsAndInputs(t *testing.T) {
	for _, name := range []string{"true", "vendor: release", "line\nbreak", `a"b`} {
		t.Run(name, func(t *testing.T) {
			observations := []Observation{
				{Input: name, Subject: plugin.SubjectSelector{Path: "Suite/Install.app"}, State: "signed", Signer: "apple:developer-id:UBF8T346G9", Name: "Publisher\nName"},
				{Input: name, Subject: plugin.SubjectSelector{Path: "Suite/Tool: 1.app"}, State: "unsigned"},
			}
			var parsed struct {
				Signatures []InputExpectation `yaml:"signatures"`
			}
			if err := yaml.Unmarshal([]byte(Fragment(observations)), &parsed); err != nil {
				t.Fatal(err)
			}
			if len(parsed.Signatures) != 2 {
				t.Fatalf("fragment lost observations: %+v", parsed)
			}
			signed, unsigned := parsed.Signatures[0], parsed.Signatures[1]
			if signed.Input != name || signed.Subject.Path != "Suite/Install.app" || signed.Signer != observations[0].Signer || unsigned.Input != name || unsigned.Subject.Path != "Suite/Tool: 1.app" || !unsigned.Unsigned {
				t.Fatalf("fragment changed observations: %+v", parsed)
			}
		})
	}
}

func TestFragmentNamesSubjectsOnlyWhereAScopeHasSeveral(t *testing.T) {
	signed := func(input, subject string) Observation {
		return Observation{Input: input, Subject: plugin.SubjectSelector{Path: subject}, State: "signed", Signer: "apple:developer-id:UBF8T346G9"}
	}
	for name, test := range map[string]struct {
		observations []Observation
		want         string
	}{
		"one application":  {[]Observation{signed("", "Example.app")}, "signatures:\n  - signer: apple:developer-id:UBF8T346G9\n"},
		"two applications": {[]Observation{signed("", "A.app"), signed("", "B.app")}, "signatures:\n  - subject:\n      path: \"A.app\"\n    signer: apple:developer-id:UBF8T346G9\n  - subject:\n      path: \"B.app\"\n    signer: apple:developer-id:UBF8T346G9\n"},
		"one per input":    {[]Observation{signed("first", "Install.app"), signed("second", "Other.app")}, "signatures:\n  - input: \"first\"\n    signer: apple:developer-id:UBF8T346G9\n  - input: \"second\"\n    signer: apple:developer-id:UBF8T346G9\n"},
	} {
		if got := Fragment(test.observations); got != test.want {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got, test.want)
		}
	}
}

func TestDeveloperIDCodeWithoutATimestampRaisesANotice(t *testing.T) {
	observed := func(input, subject string, result Result) Observation {
		result.Verifier = Verifier
		return Observation{Input: input, Subject: plugin.SubjectSelector{Path: subject}, State: "signed", Result: result}
	}
	code := Result{Signer: "apple:developer-id:UBF8T346G9", Authority: "Developer ID Application"}
	timestamped := code
	timestamped.Timestamped = true
	elsewhere := observed("", "Example.app", code)
	elsewhere.Verifier = "another.verifier/1"
	for name, test := range map[string]struct {
		observations []Observation
		hints        []string
	}{
		"untimestamped application": {[]Observation{observed("", "Example.app", code)}, []string{""}},
		"timestamped application":   {[]Observation{observed("", "Example.app", timestamped)}, nil},
		"one of two applications":   {[]Observation{observed("", "A.app", timestamped), observed("", "B.app", code)}, []string{"Subject: B.app"}},
		"build input":               {[]Observation{observed("vendor", "Install.app", code)}, []string{"Input: vendor"}},
		"one of two in an input":    {[]Observation{observed("vendor", "A.app", code), observed("vendor", "B.app", timestamped)}, []string{"Input: vendor, Subject: A.app"}},
		// A package is held to its certificate's validity when verified.
		"untimestamped package": {[]Observation{observed("", ".", Result{Signer: "apple:developer-id:UBF8T346G9", Authority: "Developer ID Installer"})}, nil},
		"App Store application": {[]Observation{observed("", "Example.app", Result{Signer: "apple:app-store:UBF8T346G9", Authority: "Mac App Store"})}, nil},
		"unsigned":              {[]Observation{{Subject: plugin.SubjectSelector{Path: "Example.app"}, State: "unsigned", Verifier: Verifier}}, nil},
		"another verifier":      {[]Observation{elsewhere}, nil},
	} {
		evidence, err := json.Marshal(test.observations)
		if err != nil {
			t.Fatal(err)
		}
		notices := Notices(plugin.ResourceReference{}, map[string]json.RawMessage{"signatures": evidence})
		if len(notices) != len(test.hints) {
			t.Errorf("%s: %+v", name, notices)
			continue
		}
		for i, notice := range notices {
			want := plugin.Notice{Level: "warning", Code: "signature-timestamp-missing", Hint: test.hints[i],
				Message: "Developer ID signature has no secure timestamp; certificate validity at signing time cannot be established"}
			if notice != want {
				t.Errorf("%s: %+v, want %+v", name, notice, want)
			}
		}
	}
}

func TestUnverifiedSubjectsRaiseOneRecommendationPerSource(t *testing.T) {
	recommend := func(kind, source string) plugin.Notice {
		return plugin.Notice{Level: "warning", Code: "signature-expectation-missing", Message: source + " has no signature expectation", Hint: "Run `stemma signature " + kind + "/example` to derive one."}
	}
	for name, test := range map[string]struct {
		kind     string
		evidence string
		want     []plugin.Notice
	}{
		"published installer":     {"WindowsSoftware", `[{"subject":{"path":"."}}]`, []plugin.Notice{recommend("WindowsSoftware", "Source")}},
		"build inputs":            {"BuildMacPkg", `[{"input":"vendor","subject":{"path":"A.app"}},{"input":"vendor","subject":{"path":"B.app"}},{"input":"tools","subject":{"path":"."}}]`, []plugin.Notice{recommend("BuildMacPkg", `Input "vendor"`), recommend("BuildMacPkg", `Input "tools"`)}},
		"nothing left unverified": {"BuildMacPkg", `[]`, nil},
	} {
		notices := Notices(plugin.ResourceReference{Kind: test.kind, Name: "example"}, map[string]json.RawMessage{UnverifiedEvidence: json.RawMessage(test.evidence), BuildEvidence: json.RawMessage(`"unsigned"`)})
		if !slices.Equal(notices, test.want) {
			t.Errorf("%s: %+v, want %+v", name, notices, test.want)
		}
	}
}

func TestExpectationsAndSchemaRequireOneState(t *testing.T) {
	schema, err := json.Marshal(plugin.SchemaFor[Expectation]())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		declaration string
		valid       bool
	}{
		{`{"subject":{"path":"."},"unsigned":true}`, true},
		{`{"subject":{"path":"."},"signer":"apple:developer-id:ABCDE12345"}`, true},
		{`{"subject":{"path":"."},"unsigned":false}`, false},
		{`{"subject":{"path":"."},"unsigned":true,"signer":"apple:developer-id:ABCDE12345"}`, false},
		{`{"subject":{"path":"."}}`, false},
		{`{"unsigned":true}`, true},
		{`{"signer":"apple:developer-id:ABCDE12345"}`, true},
	} {
		var expected Expectation
		_ = json.Unmarshal([]byte(test.declaration), &expected)
		if err := expected.Validate(AppleDeveloperID); (err == nil) != test.valid {
			t.Errorf("validation %s: %v", test.declaration, err)
		}
		if err := plugin.ValidateSchema(schema, []byte(test.declaration)); (err == nil) != test.valid {
			t.Errorf("schema %s: %v", test.declaration, err)
		}
	}
	// A subject that is declared selects something.
	if err := plugin.ValidateSchema(schema, []byte(`{"subject":{},"unsigned":true}`)); err == nil {
		t.Error("schema accepted a subject without a selector")
	}
	// The embedded assertion must not hide the builder's input field.
	schema, err = json.Marshal(plugin.SchemaFor[InputExpectation]())
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateSchema(schema, []byte(`{"input":"vendor","unsigned":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateSchema(schema, []byte(`{"subject":{"path":"."},"unsigned":true}`)); err == nil {
		t.Fatal("builder input was optional")
	}
}

func TestExpectationSignerUsesASchemeOfItsKind(t *testing.T) {
	store := Expectation{Subject: plugin.SubjectSelector{Path: "."}, Signer: "apple:app-store:ABCDE12345"}
	if err := store.Validate(AppleDeveloperID, AppleAppStore); err != nil {
		t.Fatalf("an Apple kind rejected an App Store signer: %v", err)
	}
	if err := store.Validate(Authenticode); err == nil || !strings.Contains(err.Error(), "signer must use authenticode") {
		t.Fatalf("a Windows kind accepted an App Store signer: %v", err)
	}
	windows := Expectation{Subject: plugin.SubjectSelector{Path: "."}, Signer: "authenticode:" + strings.Repeat("0a", 32)}
	if err := windows.Validate(AppleDeveloperID, AppleAppStore); err == nil || !strings.Contains(err.Error(), "signer must use apple:developer-id or apple:app-store") {
		t.Fatalf("an Apple kind accepted an Authenticode signer: %v", err)
	}
}

func TestVerifyCoverageAndStates(t *testing.T) {
	first := plugin.Subject{ID: "a", Path: "Suite/A.app", Kind: "app"}
	second := plugin.Subject{ID: "b", Path: "Suite/B.app", Kind: "app"}
	a := Expectation{Subject: plugin.SubjectSelector{Path: first.Path}, Signer: "apple:developer-id:AAAAAAAAAA"}
	b := Expectation{Subject: plugin.SubjectSelector{Path: second.Path}, Signer: "apple:developer-id:BBBBBBBBBB"}
	subjects := []plugin.Subject{first, second}
	check := func(s plugin.Subject) (Result, error) {
		if s.ID == "a" {
			return Result{Signer: a.Signer}, nil
		}
		return Result{Signer: b.Signer}, nil
	}
	if observed, err := Verify(t.Context(), []Expectation{a, b}, subjects, false, check); err != nil || len(observed) != 2 || observed[0].Signer == observed[1].Signer {
		t.Fatalf("independent signers: %+v, %v", observed, err)
	}
	for name, expectations := range map[string][]Expectation{"missing": {a}, "duplicate": {a, a, b}, "unmatched": {{Subject: plugin.SubjectSelector{Path: "missing"}, Unsigned: true}, b}} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(t.Context(), expectations, subjects, false, func(plugin.Subject) (Result, error) {
				t.Fatal("verified before resolving coverage")
				return Result{}, nil
			}); err == nil {
				t.Fatal("accepted invalid coverage")
			}
		})
	}
	// An entry that names no subject covers a scope's only subject, and names
	// the candidates where there are several. A path it does name is literal.
	only := Expectation{Signer: a.Signer}
	if observed, err := Verify(t.Context(), []Expectation{only}, subjects[:1], false, check); err != nil || len(observed) != 1 || observed[0].Subject.Path != first.Path {
		t.Fatalf("an entry without subject over one application: %+v, %v", observed, err)
	}
	if _, err := Verify(t.Context(), []Expectation{{Signer: b.Signer}}, subjects[:1], false, check); !errors.Is(err, ErrMismatch) {
		t.Fatalf("an entry without subject accepted another signer: %v", err)
	}
	if _, err := Verify(t.Context(), []Expectation{only}, subjects, false, check); err == nil || !strings.Contains(err.Error(), "signing subjects: Suite/A.app, Suite/B.app") {
		t.Fatalf("an entry without subject over applications: %v", err)
	}
	if _, err := Verify(t.Context(), []Expectation{{Subject: plugin.SubjectSelector{Path: "."}, Signer: a.Signer}}, subjects[:1], false, check); err == nil || !strings.Contains(err.Error(), "signing subjects: Suite/A.app") {
		t.Fatalf("the root path selected an application: %v", err)
	}
	unsigned := Expectation{Subject: a.Subject, Unsigned: true}
	for _, test := range []struct {
		name            string
		expected        Expectation
		result          Result
		err             error
		derive, success bool
	}{
		{name: "unsigned", expected: unsigned, err: ErrUnsigned, success: true},
		{name: "signed became unsigned", expected: a, err: ErrUnsigned},
		{name: "unsigned became signed", expected: unsigned, result: Result{Signer: a.Signer}},
		{name: "wrong signer", expected: a, result: Result{Signer: b.Signer}},
		{name: "derive changed signer", expected: a, result: Result{Signer: b.Signer}, derive: true, success: true},
		{name: "derive unsigned", expected: a, err: ErrUnsigned, derive: true, success: true},
		{name: "invalid is not unsigned", expected: unsigned, err: errors.New("invalid signature")},
		{name: "derive invalid", err: errors.New("invalid signature"), derive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Verify(t.Context(), []Expectation{test.expected}, subjects[:1], test.derive, func(plugin.Subject) (Result, error) { return test.result, test.err })
			if (err == nil) != test.success {
				t.Fatalf("verification: %v", err)
			}
		})
	}
}

func TestSignatureProgressKeepsDistinctSubjectsAndOmitsLoneRoot(t *testing.T) {
	for _, paths := range [][]string{{"."}, {"A.app"}, {".", "B.app"}} {
		t.Run(strings.Join(paths, ","), func(t *testing.T) {
			var logs bytes.Buffer
			ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)).With("input", "vendor"))
			subjects := make([]plugin.Subject, len(paths))
			for i, path := range paths {
				subjects[i] = plugin.Subject{ID: path, Path: path}
			}
			observed, err := Verify(ctx, nil, subjects, true, func(plugin.Subject) (Result, error) {
				return Result{Name: "Example Publisher"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&logs)
			for i, path := range paths {
				if observed[i].Subject.Path != path {
					t.Fatalf("observation path changed: %+v", observed[i])
				}
				for _, start := range []bool{true, false} {
					var record struct {
						Start  bool   `json:"stage"`
						Detail string `json:"detail"`
						Input  string `json:"input"`
					}
					if err := decoder.Decode(&record); err != nil {
						t.Fatal(err)
					}
					want := "Example Publisher"
					if start {
						want = path
						if path == "." && len(paths) == 1 {
							want = ""
						}
					}
					if record.Start != start || record.Detail != want || record.Input != "vendor" {
						t.Fatalf("signature activity: %+v, want %q", record, want)
					}
				}
			}
		})
	}
}
