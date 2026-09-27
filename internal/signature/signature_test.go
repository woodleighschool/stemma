package signature

import (
	"encoding/json"
	"errors"
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
		{"authenticode:" + strings.Repeat("0a", 32), Signer{Scheme: Authenticode, Value: strings.Repeat("0a", 32)}},
	} {
		signer, err := Parse(test.text)
		if err != nil || signer != test.want || signer.String() != strings.TrimSpace(test.text) {
			t.Fatalf("Parse(%q) = %+v, %v; want %+v", test.text, signer, err, test.want)
		}
	}
	for _, text := range []string{"", "UBF8T346G9", "apple:UBF8T346G9", "apple:developer-id:", "apple:developer-id:ubf8t346g9", "apple:developer-id:UBF8T346G9X", "authenticode:", "authenticode:" + strings.Repeat("0A", 32), "authenticode:" + strings.Repeat("0a", 31), "codesign:UBF8T346G9"} {
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
				{Input: name, Subject: plugin.SubjectSelector{Path: "."}, State: "unsigned"},
			}
			var parsed struct {
				Signatures []InputExpectation `yaml:"signatures"`
			}
			if err := yaml.Unmarshal([]byte(Fragment(observations)), &parsed); err != nil {
				t.Fatal(err)
			}
			if len(parsed.Signatures) != 2 || parsed.Signatures[0].Input != name || parsed.Signatures[0].Subject.Path != "Suite/Install.app" || parsed.Signatures[0].Signer != observations[0].Signer || !parsed.Signatures[1].Unsigned {
				t.Fatalf("fragment lost observations: %+v", parsed)
			}
		})
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
		{`{"unsigned":true}`, false},
		{`{"subject":{},"unsigned":true}`, false},
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
	// The embedded assertion must not hide the builder's input field.
	schema, err = json.Marshal(plugin.SchemaFor[InputExpectation]())
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateSchema(schema, []byte(`{"input":"vendor","subject":{"path":"."},"unsigned":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateSchema(schema, []byte(`{"subject":{"path":"."},"unsigned":true}`)); err == nil {
		t.Fatal("builder input was optional")
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
