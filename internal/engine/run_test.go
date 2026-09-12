package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

const policyProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: policies
spec:
  imports:
    - '*.software.yaml'
  destinations:
    first:
      operation: munki
      config:
        path: first
    second:
      operation: munki
      config:
        path: second
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: policy
spec:
  destinations:
    first:
      pkginfo:
        installer_type: nopkg
        version: '1'
        description: original
        installcheck_script: |
          #!/bin/sh
          exit 1
    second:
      pkginfo:
        installer_type: nopkg
        version: '1'
        installcheck_script: |
          #!/bin/sh
          exit 1
`

func TestSourceFreePublicationAndIndependentFailures(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	if err := testproject.Write(filename, []byte(policyProject)); err != nil {
		t.Fatal(err)
	}
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply"}
	report, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resources) != 1 || len(report.Resources[0].Artifacts) != 0 || len(report.Resources[0].Destinations) != 2 {
		t.Fatalf("incomplete sourcefree result: %+v", report)
	}
	before, err := os.ReadFile(filepath.Join(root, ".stemma/state/policies.json"))
	if err != nil {
		t.Fatal(err)
	}
	options.Method = "plan"
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, ".stemma/state/policies.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("plan changed durable bindings")
	}
	if err := testproject.Write(filename, []byte(strings.Replace(policyProject, "description: original", "description: edited", 1))); err != nil {
		t.Fatal(err)
	}
	options.Method = "apply"
	report, err = Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Resources[0].Cached {
		t.Fatal("metadata invalidated sourcefree preparation")
	}
	applied := []string{}
	options.Handlers = map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
		if request.Method == "plan" && request.Identity.Destination == "first" {
			return plugin.ReconcileResponse{}, errors.New("unavailable")
		}
		if request.Method == "apply" {
			applied = append(applied, request.Identity.Destination)
		}
		return plugin.ReconcileResponse{}, nil
	}}
	report, err = Run(t.Context(), options)
	if err == nil || len(applied) != 1 || applied[0] != "second" || !report.Resources[0].Destinations[1].Applied {
		t.Fatalf("independent destination was blocked: %+v %v", report, err)
	}
}

func TestPartialFailurePersistsOwnedBindingWithoutSuccess(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	if err := testproject.Write(filename, []byte(policyProject)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("upload failed")
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply", Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
		if request.Method == "apply" {
			return plugin.ReconcileResponse{Binding: json.RawMessage(`{"id":"owned-staging"}`)}, failure
		}
		return plugin.ReconcileResponse{}, nil
	}}}
	if _, err := Run(t.Context(), options); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	current, err := loadState(filepath.Join(root, ".stemma/state/policies.json"), "policies")
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range current.Bindings {
		if compactJSON(t, binding.Binding) != `{"id":"owned-staging"}` || binding.Payload != "" {
			t.Fatal("failed staging displaced successful payload or lost cleanup binding")
		}
	}
}

func TestNativeValidationBeforeAcquisition(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	manifest := strings.Replace(policyProject, "  destinations:\n    first:\n      pkginfo:", "  source:\n    path: missing.pkg\n  destinations:\n    first:\n      pkginfo:", 1)
	manifest = strings.Replace(manifest, "description: original", "unattended_install: invalid", 1)
	if err := testproject.Write(filename, []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	_, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply"})
	if err == nil || !strings.Contains(err.Error(), "unattended_install") {
		t.Fatalf("invalid native field reached acquisition: %v", err)
	}
}

func TestOpenEvidencePreservesNativeTypes(t *testing.T) {
	effective, _, err := resolveMetadata(plugin.ResourceResult{}, map[string]any{"count": map[string]any{"$fact": "vendor.probe.count"}, "enabled": map[string]any{"$fact": "vendor.probe.enabled"}}, plugin.Facts{}, map[string]json.RawMessage{"vendor.probe": json.RawMessage(`{"count":4,"enabled":false}`)})
	if err != nil || effective["count"] != float64(4) || effective["enabled"] != false {
		t.Fatalf("evidence handoff: %+v %v", effective, err)
	}
	if _, _, err := resolveMetadata(plugin.ResourceResult{}, map[string]any{"value": map[string]any{"$fact": "vendor.missing.value"}}, plugin.Facts{}, nil); err == nil {
		t.Fatal("missing evidence accepted")
	}
}

func compactJSON(t *testing.T, data json.RawMessage) string {
	t.Helper()
	if len(data) == 0 {
		return ""
	}
	var result bytes.Buffer
	if err := json.Compact(&result, data); err != nil {
		t.Fatal(err)
	}
	return result.String()
}

func TestSourceFreeCannotSilentlySkipVerification(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	manifest := strings.Replace(policyProject, "spec:\n  destinations:\n    first:\n      pkginfo:", "spec:\n  verification:\n    signature: true\n  destinations:\n    first:\n      pkginfo:", 1)
	if err := testproject.Write(filename, []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply"}); err == nil || !strings.Contains(err.Error(), "require a source") {
		t.Fatalf("sourcefree verification was skipped: %v", err)
	}
}
