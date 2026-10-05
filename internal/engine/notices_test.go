package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/woodleighschool/stemma/internal/testutil/testproject"
)

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
