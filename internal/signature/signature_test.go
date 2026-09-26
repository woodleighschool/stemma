package signature

import (
	"errors"
	"strings"
	"testing"
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
	result := Result{Signer: observed.String(), Name: "Microsoft\nCorporation", Target: "installer.pkg"}
	if got := result.Fragment(); got != "signature:\n  signer: apple:developer-id:UBF8T346G9 # Microsoft Corporation\n" {
		t.Fatalf("fragment: %q", got)
	}
	if got := (Result{Signer: observed.String()}).Fragment(); got != "signature:\n  signer: apple:developer-id:UBF8T346G9\n" {
		t.Fatalf("fragment without name: %q", got)
	}
	input := InputResult{Input: "vendor", Result: result}
	if got := input.Fragment(); got != "signature:\n  input: vendor\n  signer: apple:developer-id:UBF8T346G9 # Microsoft Corporation\n" {
		t.Fatalf("input fragment: %q", got)
	}
}
