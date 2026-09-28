package apple

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/signature"
)

// TestNestedCodeReplacedUnderItsRequirement swaps re-signed helpers into
// RequirementFixture.app, whose Contents/Helpers/helper records the Developer
// ID requirement and Contents/Helpers/weak a weaker one.
func TestNestedCodeReplacedUnderItsRequirement(t *testing.T) {
	for _, test := range []struct {
		name, slot, replacement, rejection string
		codesign                           bool
	}{
		// The weak slot's exact code passes without evaluating its requirement.
		{name: "sealed code", codesign: true},
		{name: "re-signed", slot: "helper", replacement: "resigned", codesign: true},
		{name: "another identifier", slot: "helper", replacement: "renamed", rejection: "matches neither"},
		{name: "ad-hoc", slot: "helper", replacement: "adhoc", rejection: "no CMS signature"},
		// codesign accepts what a weaker requirement allows; Stemma evaluates
		// only the Developer ID form.
		{name: "weaker requirement", slot: "weak", replacement: "resigned", rejection: "not the Developer ID form", codesign: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := copyFixture(t, "RequirementFixture.app")
			if test.slot != "" {
				writeTestFile(t, filepath.Join(app, "Contents/Helpers", test.slot), readTestFile(t, filepath.Join("testdata/replacements", test.replacement)), 0o755)
			}
			result, err := VerifyApp(t.Context(), app, signature.Signer{})
			switch {
			case test.rejection != "":
				if err == nil || !strings.Contains(err.Error(), test.rejection) {
					t.Fatalf("got %v, want %s", err, test.rejection)
				}
			case err != nil:
				t.Fatal(err)
			case test.slot == "":
				if len(result.Replaced) != 0 {
					t.Fatalf("sealed code reported as replaced: %+v", result.Replaced)
				}
			default:
				if len(result.Replaced) != 1 {
					t.Fatalf("replaced = %+v", result.Replaced)
				}
				replaced := result.Replaced[0]
				if replaced.Path != "Contents/Helpers/"+test.slot || len(replaced.Sealed) != 40 || len(replaced.CDHashes) == 0 || slices.Contains(replaced.CDHashes, replaced.Sealed) {
					t.Fatalf("replacement evidence: %+v", replaced)
				}
			}
			if runtime.GOOS != "darwin" {
				return
			}
			output, err := exec.CommandContext(t.Context(), "/usr/bin/codesign", "--verify", "--strict", "--deep", app).CombinedOutput()
			if (err == nil) != test.codesign {
				t.Fatalf("codesign accept = %v, want %v: %s", err == nil, test.codesign, output)
			}
		})
	}
}

func TestParseRequirement(t *testing.T) {
	const developerID = ` and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */ and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */ and certificate leaf[subject.OU] = `
	for _, test := range []struct{ name, text, identifier, team, rejection string }{
		{"Cura", `identifier "_pynavlib.cpython-312-darwin"` + developerID + `V4B3JXRRQS`, "_pynavlib.cpython-312-darwin", "V4B3JXRRQS", ""},
		{"VLC", `identifier deprecated` + developerID + `"75GAHG3SZQ"`, "deprecated", "75GAHG3SZQ", ""},
		{"explicit operators", `identifier = "org.example.app" and anchor apple generic and cert 1[field.1.2.840.113635.100.6.2.6] exists and cert 0[field.1.2.840.113635.100.6.1.13] exists and cert leaf[subject.OU] = ABCDE12345`, "org.example.app", "ABCDE12345", ""},
		{"quoted keyword", `identifier "a and b \"c\""` + developerID + `ABCDE12345`, `a and b "c"`, "ABCDE12345", ""},
		{"hexadecimal team", `identifier "org.example.app"` + developerID + `0x41424344453132333435`, "", "", "clause"},
		{"bare number", `identifier "org.example.app"` + developerID + `12345`, "", "", "clause"},
		{"reserved word", `identifier always` + developerID + `ABCDE12345`, "", "", "clause"},
		{"unsupported escape", `identifier "org.example\q"` + developerID + `ABCDE12345`, "", "", "escape"},
		{"no team", `identifier "org.example.app" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] and certificate leaf[field.1.2.840.113635.100.6.1.13]`, "", "", "no team clause"},
		{"no anchor", `identifier "org.example.app" and certificate 1[field.1.2.840.113635.100.6.2.6] and certificate leaf[field.1.2.840.113635.100.6.1.13] and certificate leaf[subject.OU] = ABCDE12345`, "", "", "no anchor clause"},
		{"alternative", `identifier "org.example.app" or identifier "org.example.other"` + developerID + `ABCDE12345`, "", "", "clause"},
		{"grouping", `(identifier "org.example.app")` + developerID + `ABCDE12345`, "", "", "clause"},
		{"negation", `! identifier "org.example.app"` + developerID + `ABCDE12345`, "", "", "clause"},
		{"Apple's own code", `identifier "org.example.app" and anchor apple`, "", "", "clause"},
		{"other certificate", `identifier "org.example.app"` + developerID + `ABCDE12345 and certificate root[field.1.2.840.113635.100.6.2.6]`, "", "", "clause"},
		{"intermediate in another position", strings.Replace(`identifier "org.example.app"`+developerID+`ABCDE12345`, "certificate 1[", "certificate 2[", 1), "", "", "clause"},
		{"information property", `identifier "org.example.app"` + developerID + `ABCDE12345 and info[CFBundleVersion] = "1.0"`, "", "", "clause"},
		{"repeated identifier", `identifier "org.example.app" and identifier "org.example.other"` + developerID + `ABCDE12345`, "", "", "repeats"},
		{"wildcard", `identifier "org.example.app"` + developerID + `"ABCDE"*`, "", "", "unexpected character"},
		{"empty clause", `identifier "org.example.app" and` + developerID + `ABCDE12345`, "", "", "clause"},
		{"unterminated string", `identifier "org.example.app`, "", "", "unterminated string"},
		{"unterminated comment", `identifier "org.example.app" /* exists`, "", "", "unterminated comment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, err := parseRequirement(test.text)
			if test.rejection != "" {
				if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), test.rejection) {
					t.Fatalf("got %v, want unsupported %s", err, test.rejection)
				}
				return
			}
			if err != nil || r.identifier != test.identifier || r.team != test.team {
				t.Fatalf("got %+v, %v", r, err)
			}
		})
	}
}

func TestRequirementNeedsEveryDeveloperIDProperty(t *testing.T) {
	r := requirement{identifier: "org.example.helper", team: "ABCDE12345"}
	valid := codeIdentity{identifier: "org.example.helper", teamID: "ABCDE12345", application: true, developerIDCA: true}
	if !r.satisfiedBy(valid) {
		t.Fatal("Developer ID code failed its requirement")
	}
	for name, change := range map[string]func(*codeIdentity){
		"identifier":   func(c *codeIdentity) { c.identifier = "org.example.other" },
		"team":         func(c *codeIdentity) { c.teamID = "FGHIJ67890" },
		"intermediate": func(c *codeIdentity) { c.developerIDCA = false },
		"application":  func(c *codeIdentity) { c.application = false },
	} {
		identity := valid
		change(&identity)
		if r.satisfiedBy(identity) {
			t.Fatalf("code with another %s satisfied the requirement", name)
		}
	}
}
