package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

// Signing evidence is stored with an output, so the advisory it raises is the
// same whether preparation verified the signature or reused the output.
func TestSigningEvidenceNoticesSurvivePreparationCache(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "local-plugin", "plugin")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	observed := []signature.Observation{{Subject: plugin.SubjectSelector{Path: "Example.app"}, State: "signed", Signer: "apple:developer-id:UBF8T346G9", Authority: "Developer ID Application", Verifier: signature.Verifier}}
	evidence, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	testproject.Write(t, filepath.Join(root, "stemma.yaml"), fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: evidence}
spec:
  imports: ['*.software.yaml']
  plugins:
    provider: {path: local-plugin}
---
apiVersion: example.test/v1
kind: ExternalInstaller
metadata: {name: fixture}
spec:
  source: {path: vendor.pkg}
  evidence: {signatures: %s}
`, evidence))
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), []byte("vendor"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: t.TempDir(), Method: "update"}
	if _, err := UpdatePlugins(t.Context(), options, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "prepare"
	for attempt := range 2 {
		report, err := Run(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		resource := report.Resources[0]
		if len(resource.Notices) != 1 || resource.Notices[0].Code != "signature-timestamp-missing" || resource.Cached != (attempt == 1) {
			t.Fatalf("attempt %d: %+v", attempt, resource)
		}
	}
}

func TestDeclarationNoticesSurvivePreparationCache(t *testing.T) {
	root := t.TempDir()
	testproject.Write(t, filepath.Join(root, "stemma.yaml"), `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: notices}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: vendor}
spec:
  source: {path: vendor.pkg}
`)
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	options := Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "prepare"
	var previous ResourceReport
	for attempt := range 2 {
		var streamed []ResourceReport
		options.ResourceDone = func(resource ResourceReport) error { streamed = append(streamed, resource); return nil }
		report, err := Run(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Resources) != 1 {
			t.Fatalf("report=%+v", report)
		}
		resource := report.Resources[0]
		if len(resource.Notices) != 1 || resource.Notices[0].Code != "signature-expectation-missing" || resource.Cached != (attempt == 1) {
			t.Fatalf("attempt %d: %+v", attempt, resource)
		}
		if !reflect.DeepEqual(streamed, report.Resources) {
			t.Fatal("streamed notices differ from report")
		}
		if attempt == 1 && !reflect.DeepEqual(previous.Notices, resource.Notices) {
			t.Fatal("cache changed notices")
		}
		previous = resource
	}
	options.Method = "signature"
	report, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resources[0].Notices) != 0 {
		t.Fatal("signature derivation recommended itself")
	}
}
