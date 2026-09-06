package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
)

func TestPreparationVerifiesAndReportsDefaultPayload(t *testing.T) {
	for _, policy := range []string{"signature: true", "integrity: true", "subject: payload, integrity: true"} {
		t.Run(policy, func(t *testing.T) {
			root := t.TempDir()
			payload := filepath.Join(root, "Payload")
			if err := os.Mkdir(payload, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(payload, "example.txt"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := pkgbuild.Build(t.Context(), payload, filepath.Join(root, "fixture.pkg"), pkgbuild.Options{Identifier: "org.example.fixture", Version: "1", Payload: "."}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "stemma.yaml")
			manifest := fmt.Sprintf("version: 1\nproject: verify-payload\nrecipes:\n  example:\n    source: {type: file, path: fixture.pkg}\n    verification: {%s}\n", policy)
			if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := Options{ConfigPath: path, CacheDir: t.TempDir(), Method: "prepare"}
			for range 2 {
				report, err := Run(t.Context(), opts)
				if policy == "signature: true" {
					if err == nil {
						t.Fatal("unsigned payload passed required signature verification")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				prepared := report.Recipes[0].Prepared
				if prepared.Evidence == nil || prepared.Evidence.Integrity.Status != apple.Valid || prepared.Evidence.SubjectSHA256 != prepared.Payload.SHA256 {
					t.Fatalf("verification evidence was lost: %+v", prepared.Evidence)
				}
			}
		})
	}
}

func TestDocumentDeliveryVerifiesTheExplicitInstallerOutput(t *testing.T) {
	for _, subject := range []string{"build/artifact", "metadata/artifact", "build/missing"} {
		t.Run(subject, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "Payload"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "Payload", "example.txt"), []byte("synthetic package payload"), 0o600); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf(`version: 1
project: verify-document
recipes:
  example:
    source: {type: file, path: Payload}
    verification: {subject: %s, integrity: true}
    steps:
      - name: build
        operation: pkg
        inputs: {input: prepared}
        config: {identifier: org.example.fixture, version: "1", payload: .}
      - name: metadata
        operation: munki.pkginfo
        inputs: {input: build/artifact}
        config: {name: Example}
    destinations:
      local: {artifact: metadata/artifact, inputs: {installer: build/artifact}}
destinations:
  local: {operation: munki, path: repo}
`, subject)
			configPath := filepath.Join(root, "stemma.yaml")
			if err := os.WriteFile(configPath, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := Options{ConfigPath: configPath, CacheDir: t.TempDir(), Method: "apply"}
			report, err := Run(t.Context(), opts)
			if subject != "build/artifact" {
				if err == nil || !strings.Contains(err.Error(), "verification subject") {
					t.Fatalf("wrong or absent verification output did not block publication: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "repo")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed verification changed the destination: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, report := range []Report{report, runVerifiedAgain(t, opts)} {
				installer := report.Recipes[0].Steps[0].Artifacts["artifact"]
				document := report.Recipes[0].Steps[1].Artifacts["artifact"]
				if installer.Evidence == nil || installer.Evidence.Integrity.Status != apple.Valid || installer.Evidence.SubjectSHA256 != installer.Payload.SHA256 {
					t.Fatalf("evidence was not bound to the chosen installer: %+v", installer.Evidence)
				}
				if document.Evidence != nil {
					t.Fatal("installer evidence was attached to the metadata document")
				}
			}
		})
	}
}

func runVerifiedAgain(t *testing.T, opts Options) Report {
	t.Helper()
	report, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return report
}
