package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	var completed []ResourceReport
	options.ResourceDone = func(result ResourceReport) error {
		if len(result.Destinations) != 2 || len(applied) != 1 {
			t.Fatalf("resource completed before independent destinations: %+v", result)
		}
		completed = append(completed, result)
		return nil
	}
	var logs bytes.Buffer
	ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
	report, err = Run(ctx, options)
	if err == nil || len(applied) != 1 || applied[0] != "second" || !report.Resources[0].Destinations[1].Applied {
		t.Fatalf("independent destination was blocked: %+v %v", report, err)
	}
	if !reflect.DeepEqual(completed, report.Resources) {
		t.Fatalf("streamed results differ from final report: %+v", completed)
	}
	failed := false
	for line := range bytes.SplitSeq(bytes.TrimSpace(logs.Bytes()), []byte{'\n'}) {
		var record struct {
			Message     string `json:"msg"`
			Destination string `json:"destination"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Destination == "first" {
			failed = failed || record.Message == "Destination failed"
			if record.Message == "Destination planned" || record.Message == "Destination applied" {
				t.Fatalf("failed destination logged success: %s", line)
			}
		}
	}
	if !failed {
		t.Fatal("failed destination was not logged")
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

func TestPrepareFinishesEachResourceBeforeAcquiringTheNext(t *testing.T) {
	for _, cancelDuringValidation := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelDuringValidation), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			var events []string
			record := func(event string) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, event)
			}
			installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record("acquire " + r.URL.Path)
				_, _ = w.Write(installer)
			}))
			defer server.Close()
			root := t.TempDir()
			manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: sequential}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
`
			for _, name := range []string{"a", "b"} {
				manifest += fmt.Sprintf(`---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: %s}
spec:
  source: {url: %s/%s.pkg}
  verification: {integrity: true}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`, name, server.URL, name)
			}
			filename := filepath.Join(root, "stemma.yaml")
			if err := testproject.Write(filename, []byte(manifest)); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			ctx = plugin.WithLogger(ctx, slog.New(slog.NewJSONHandler(&logs, nil)))
			report, err := Run(ctx, Options{
				ConfigPath: filename, CacheDir: t.TempDir(), Method: "prepare",
				Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
					if request.Prepared {
						record("validate " + request.Identity.Software)
						if cancelDuringValidation {
							cancel()
							return plugin.ReconcileResponse{}, ctx.Err()
						}
					}
					return plugin.ReconcileResponse{}, nil
				}},
				ResourceDone: func(result ResourceReport) error { record("done " + result.Name); return nil },
			})
			mu.Lock()
			defer mu.Unlock()
			if cancelDuringValidation {
				if !errors.Is(err, context.Canceled) || len(report.Resources) != 1 || report.LockChanged != nil {
					t.Fatalf("cancellation did not stop the run: %+v %v", report, err)
				}
				if got := strings.Join(events, ", "); got != "acquire /a.pkg, validate a" {
					t.Fatalf("work continued after cancellation: %s", got)
				}
				if strings.Contains(logs.String(), "Preparation failed") || strings.Contains(logs.String(), "Destination failed") {
					t.Fatalf("cancellation logged ordinary failures: %s", logs.String())
				}
				if _, err := os.Stat(filepath.Join(root, "stemma.lock.yaml")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("cancelled run wrote a partial lockfile")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Join(events, ", "); got != "acquire /a.pkg, validate a, done a, acquire /b.pkg, validate b, done b" {
					t.Fatalf("resource work was split into passes: %s", got)
				}
			}
		})
	}
}

func TestApplyKeepsCrossedPublicationDependenciesIndependent(t *testing.T) {
	parts := strings.Split(policyProject, "\n---\n")
	manifest := parts[0]
	for _, name := range []string{"a", "b"} {
		manifest += "\n---\n" + strings.Replace(parts[1], "name: policy", "name: "+name, 1)
	}
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	if err := testproject.Write(filename, []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	var applied []string
	report, err := Run(t.Context(), Options{
		ConfigPath: filename, CacheDir: t.TempDir(), Method: "apply",
		Handlers: map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
			identity := request.Identity.Software + "/" + request.Identity.Destination
			if request.Method == "validate" && !request.Prepared {
				switch identity {
				case "a/first":
					return plugin.ReconcileResponse{Requires: []string{"b"}}, nil
				case "b/second":
					return plugin.ReconcileResponse{Requires: []string{"a"}}, nil
				}
			}
			if request.Method == "apply" {
				applied = append(applied, identity)
			}
			return plugin.ReconcileResponse{}, nil
		}},
	})
	if err != nil || len(report.Resources) != 2 {
		t.Fatalf("crossed destinations blocked execution: %+v %v", report, err)
	}
	if got := strings.Join(applied, ", "); got != "b/first, a/first, a/second, b/second" {
		t.Fatalf("publication dependencies lost: %s", got)
	}
}

func TestApplyChecksEveryReviewedInputBeforeWriting(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	parts := strings.Split(policyProject, "\n---\n")
	manifest := parts[0]
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, name+".pkg"), installer, 0o644); err != nil {
			t.Fatal(err)
		}
		resource := strings.Replace(parts[1], "name: policy", "name: "+name, 1)
		resource = strings.Replace(resource, "spec:\n", "spec:\n  source: {path: "+name+".pkg}\n", 1)
		manifest += "\n---\n" + resource
	}
	if err := testproject.Write(filename, []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.pkg"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	writes := 0
	options.Method = "apply"
	options.Handlers = map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
		if request.Method == "apply" {
			writes++
		}
		return plugin.ReconcileResponse{}, nil
	}}
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "local input content changed") || writes != 0 {
		t.Fatalf("apply wrote before checking the later resource: writes=%d error=%v", writes, err)
	}
}
