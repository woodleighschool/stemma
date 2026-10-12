package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestDestinationContentIsVerifiedAndScopedToInvocation(t *testing.T) {
	for _, tree := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "tree"}[tree], func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			source := t.TempDir()
			name := filepath.Join(source, "data")
			if err := os.WriteFile(name, []byte("original"), 0640); err != nil {
				t.Fatal(err)
			}
			if !tree {
				source = name
			}
			ref, err := importPath(t.Context(), store, source, tree)
			if err != nil {
				t.Fatal(err)
			}
			artifact := plugin.Artifact{SHA256: ref.SHA256, Size: ref.Size, Filename: "installer", Mode: 0750, Tree: tree}
			registry := plugin.New("fixture", "1")
			var paths []string
			handler := func(ctx context.Context, req plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
				if req.Artifact.Path == "" {
					return plugin.ReconcileResponse{Content: &plugin.ContentRequest{Artifact: true}}, nil
				}
				got, err := digestPath(ctx, req.Artifact.Path, tree)
				if err != nil || got != ref {
					t.Fatalf("content=%+v err=%v", got, err)
				}
				paths = append(paths, req.Artifact.Path)
				path := req.Artifact.Path
				if tree {
					path = filepath.Join(path, "data")
				}
				// A misbehaving consumer cannot poison a later invocation or the CAS.
				return plugin.ReconcileResponse{}, os.WriteFile(path, []byte("modified"), 0640)
			}
			err = plugin.Register(registry, plugin.Operation{Name: "fixture", Kind: "reconcile", Methods: []string{"validate", "plan", "apply"}, SideEffects: "remote"}, handler)
			if err != nil {
				t.Fatal(err)
			}
			e := execution{session: &session{store: store, ops: &operations{registry: registry}}}
			work := t.TempDir()
			for _, method := range []string{"validate", "plan", "apply"} {
				_, err = e.callDestination(t.Context(), "fixture", plugin.ReconcileRequest[json.RawMessage]{Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "Fixture", Name: "example"}}, Method: method, Artifact: artifact}, work)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(paths[len(paths)-1]); !os.IsNotExist(err) {
					t.Fatalf("lease survives invocation: %v", err)
				}
			}
			if err := store.Verify(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			object, _ := store.Path(ref)
			if err := os.Chmod(object, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(object, []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = e.callDestination(t.Context(), "fixture", plugin.ReconcileRequest[json.RawMessage]{Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "Fixture", Name: "example"}}, Method: "apply", Artifact: artifact}, work)
			if err == nil || len(paths) != 3 {
				t.Fatalf("corrupt content reached consumer: calls=%d err=%v", len(paths), err)
			}
		})
	}
}

func TestDestinationContentRequestsMustBeSatisfiable(t *testing.T) {
	for _, test := range []struct {
		name    string
		content plugin.ContentRequest
		want    string
	}{
		{"empty", plugin.ContentRequest{}, "requested no content"},
		{"absent artifact", plugin.ContentRequest{Artifact: true}, "unavailable content"},
		{"unknown input", plugin.ContentRequest{Inputs: []string{"unknown"}}, "unknown input"},
		{"repeated", plugin.ContentRequest{Inputs: []string{"installer"}}, "requested content again"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ref, err := store.Import(t.Context(), strings.NewReader("installer"), "")
			if err != nil {
				t.Fatal(err)
			}
			registry := plugin.New("fixture", "1")
			err = plugin.Register(registry, plugin.Operation{Name: "fixture", Kind: "reconcile", Methods: []string{"validate", "plan", "apply"}, SideEffects: "remote"}, func(_ context.Context, _ plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
				return plugin.ReconcileResponse{Content: &test.content}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			e := execution{session: &session{store: store, ops: &operations{registry: registry}}}
			req := plugin.ReconcileRequest[json.RawMessage]{Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "Fixture", Name: "example"}}, Method: "apply", Inputs: map[string]plugin.Artifact{"installer": {SHA256: ref.SHA256, Size: ref.Size, Filename: "installer.pkg", Mode: 0644}}}
			_, err = e.callDestination(t.Context(), "fixture", req, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestWarmReconciliationDefersPayloadIntegrityUntilConsumption(t *testing.T) {
	server := httptest.NewServer(http.FileServer(http.Dir("../apple/testdata")))
	defer server.Close()
	project := filepath.Join(t.TempDir(), "stemma.yaml")
	testproject.Write(t, project, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: fixture}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: unused}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: fixture}
spec:
  source: {url: %q}
  destinations:
    repo: {pkginfo: {description: Managed}}
`, server.URL+"/fixture.pkg"))
	consume := false
	opts := Options{ConfigPath: project, CacheDir: t.TempDir(), Method: "update", Handlers: map[string]reconcileHandler{"munki": func(ctx context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		if consume && request.Method == "apply" {
			if request.Artifact.Path == "" {
				return plugin.ReconcileResponse{Content: &plugin.ContentRequest{Artifact: true}}, nil
			}
			ref, err := digestPath(ctx, request.Artifact.Path, false)
			if err != nil || ref.SHA256 != request.Artifact.SHA256 {
				t.Fatalf("publication content=%+v err=%v", ref, err)
			}
			return plugin.ReconcileResponse{}, nil
		}
		if request.Artifact.Path != "" {
			t.Fatal("metadata reconciliation received bytes")
		}
		return plugin.ReconcileResponse{Metadata: request.Metadata}, nil
	}}}
	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	opts.Method = "prepare"
	report, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	payload := report.Resources[0].Artifacts["installer"].Payload
	store, err := cas.Open(opts.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	name, err := store.Path(payload)
	if err != nil {
		t.Fatal(err)
	}
	// Leave the descriptor and size intact. Metadata work must not certify or
	// depend on the unused bytes; the first real consumer must reject them.
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte("bad!"), 0)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"plan", "apply"} {
		opts.Method = method
		if _, err := Run(t.Context(), opts); err != nil {
			t.Fatalf("metadata %s: %v", method, err)
		}
	}
	consume = true
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "integrity verification") {
		t.Fatalf("consumption error=%v", err)
	}
	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatalf("retry did not reacquire discarded content: %v", err)
	}
}
