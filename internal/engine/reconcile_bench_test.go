package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

// BenchmarkWarmReconcile includes startup, locked acquisition and preparation
// recall. Destination-specific benchmarks measure metadata and publication work.
func BenchmarkWarmReconcile(b *testing.B) {
	payload := b.TempDir()
	data := make([]byte, 4<<20)
	random := rand.NewChaCha8([32]byte{})
	if _, err := random.Read(data); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, "data"), data, 0644); err != nil {
		b.Fatal(err)
	}
	installer := filepath.Join(b.TempDir(), "benchmark.pkg")
	if err := pkgbuild.Build(b.Context(), payload, installer, pkgbuild.Options{Identifier: "org.example.benchmark", Version: "1.0", Payload: ".", Compression: pkgbuild.Gzip, InstallLocation: "/Library/Benchmark"}); err != nil {
		b.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, installer) }))
	defer server.Close()
	project := filepath.Join(b.TempDir(), "stemma.yaml")
	testproject.Write(b, project, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: benchmark}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: unused}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: benchmark}
spec:
  source: {url: %q}
  destinations:
    repo: {pkginfo: {description: Managed}}
`, server.URL+"/benchmark.pkg"))
	handler := func(_ context.Context, req plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		if req.Artifact.Path != "" {
			b.Fatal("metadata reconciliation received installer bytes")
		}
		return plugin.ReconcileResponse{Metadata: req.Metadata}, nil
	}
	opts := Options{ConfigPath: project, CacheDir: b.TempDir(), Method: "update", Handlers: map[string]reconcileHandler{"munki": handler}}
	if _, err := Run(b.Context(), opts); err != nil {
		b.Fatal(err)
	}
	opts.Method = "apply"
	if _, err := Run(b.Context(), opts); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Run(b.Context(), opts); err != nil {
			b.Fatal(err)
		}
	}
}
