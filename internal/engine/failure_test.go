package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestRunReportsFailuresAndBlockedConsumers(t *testing.T) {
	for _, method := range []string{"update", "prepare", "plan", "apply", "icon"} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"producer", "healthy"} {
				if err := os.WriteFile(filepath.Join(root, name+".pkg"), installer, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: failures}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
`
			for _, resource := range []struct{ name, source string }{
				{"a", "{path: producer.pkg}"},
				{"b", "{resource: {kind: MacSoftware, name: a}}"},
				{"c", "{resource: {kind: MacSoftware, name: b}}"},
				{"z", "{path: healthy.pkg}"},
			} {
				manifest += fmt.Sprintf(`---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: %s}
spec:
  source: %s
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`, resource.name, resource.source)
			}
			filename := filepath.Join(root, "stemma.yaml")
			if method == "icon" {
				manifest = strings.ReplaceAll(manifest, "  source:", "  icon: fixture\n  source:")
			}
			testproject.Write(t, filename, manifest)
			opts := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update", Icons: IconOptions{Presentation: icon.Raw}}
			if _, err := Run(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(lockfile.Filename(root))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "producer.pkg")); err != nil {
				t.Fatal(err)
			}
			var completed []ResourceReport
			var published []string
			opts.Method = method
			opts.ResourceDone = func(resource ResourceReport) error {
				completed = append(completed, resource)
				return nil
			}
			opts.Handlers = map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
				if request.Method == "apply" {
					published = append(published, request.Identity.Resource.Name)
				}
				return plugin.ReconcileResponse{}, nil
			}}
			report, err := Run(t.Context(), opts)
			if err == nil || len(report.Resources) != 4 || !reflect.DeepEqual(completed, report.Resources) {
				t.Fatalf("run did not complete each resource: %+v, completed=%+v, err=%v", report, completed, err)
			}
			if report.Resources[0].Error == "" || len(report.Resources[0].BlockedBy) != 0 {
				t.Fatalf("producer did not fail: %+v", report.Resources[0])
			}
			for i := 1; i <= 2; i++ {
				producer := report.Resources[i-1].Key
				if !slices.Equal(report.Resources[i].BlockedBy, []string{producer}) || !strings.Contains(report.Resources[i].Error, producer) {
					t.Fatalf("consumer lost its blocker: %+v", report.Resources[i])
				}
			}
			if strings.Contains(err.Error(), "blocked by") || report.Resources[3].Error != "" {
				t.Fatalf("blocked or healthy resource counted as an independent failure: %+v, %v", report, err)
			}
			if method == "apply" && !slices.Equal(published, []string{"z"}) {
				t.Fatalf("independent resource was not published: %v", published)
			}
			after, err := os.ReadFile(lockfile.Filename(root))
			if err != nil || string(before) != string(after) || report.LockChanged != nil && *report.LockChanged {
				t.Fatalf("failed run changed reviewed entries: %+v, %v", report, err)
			}
			if method != "icon" && report.LockChanged == nil {
				t.Fatal("lock comparison did not complete")
			}
			candidate, err := Resolve(t.Context(), opts)
			if err != nil || candidate.Resources[report.Resources[0].Key].Error == "" || candidate.Resources[report.Resources[3].Key].Error != "" {
				t.Fatalf("candidate disagrees with execution: %+v, %v", candidate, err)
			}
			for i := 1; i <= 2; i++ {
				if !slices.Equal(candidate.Resources[report.Resources[i].Key].BlockedBy, report.Resources[i].BlockedBy) {
					t.Fatalf("candidate lost blocker: %+v", candidate)
				}
			}
		})
	}
}

func TestPreparationFailureRetainsReviewedInputs(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: failures}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: broken}
spec:
  source: {path: vendor.pkg}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: consumer}
spec:
  source: {resource: {kind: MacSoftware, name: broken}}
`
	testproject.Write(t, filename, manifest)
	opts := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	before, err := lockfile.Load(lockfile.Filename(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), []byte("invalid package"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "healthy.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest += `---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: healthy}
spec:
  source: {path: healthy.pkg}
`
	testproject.Write(t, filename, manifest)
	opts.Method = "prepare"
	report, err := Run(t.Context(), opts)
	if err == nil || len(report.Resources) != 3 || report.Resources[0].Error == "" || len(report.Resources[1].BlockedBy) != 1 || report.Resources[2].Error != "" || report.LockChanged == nil || !*report.LockChanged {
		t.Fatalf("preparation failure did not stay local: %+v, %v", report, err)
	}
	after, err := lockfile.Load(lockfile.Filename(root))
	const broken = "stemma/v1alpha1/MacSoftware/broken"
	if err != nil || !reflect.DeepEqual(before.Inputs[broken], after.Inputs[broken]) || len(after.Inputs) != 2 {
		t.Fatalf("failed preparation committed new input bytes: %+v, %v", after, err)
	}
}

func TestRunAbortsOnCancellationOrReportFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			parts := strings.Split(policyProject, "\n---\n")
			filename := filepath.Join(t.TempDir(), "stemma.yaml")
			testproject.Write(t, filename, policyProject+"\n---\n"+strings.Replace(parts[1], "name: policy", "name: second", 1))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("report unavailable")
			if canceled {
				failure = context.Canceled
			}
			report, err := Run(ctx, Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update", ResourceDone: func(ResourceReport) error {
				if canceled {
					cancel()
					return nil
				}
				return failure
			}})
			if !errors.Is(err, failure) || len(report.Resources) != 1 || report.LockChanged != nil {
				t.Fatalf("global failure did not abort immediately: %+v, %v", report, err)
			}
		})
	}
}
