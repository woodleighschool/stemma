package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
)

// STEMMA_BENCH_APP selects a read-only installed bundle. Each benchmark owns
// its project and caches and has no destinations. Cold means an empty Stemma
// cache, not a flushed operating-system filesystem cache.
func BenchmarkPrepareApplication(b *testing.B) {
	app := os.Getenv("STEMMA_BENCH_APP")
	if app == "" {
		app = "../apple/testdata/NestedFixture.app"
	}
	app, err := filepath.Abs(app)
	if err != nil {
		b.Fatal(err)
	}
	verified, err := apple.VerifyApp(b.Context(), app, signature.Signer{})
	if err != nil {
		b.Fatal(err)
	}
	project := filepath.Join(b.TempDir(), "stemma.yaml")
	testproject.Write(b, project, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: benchmark}
spec:
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: application}
spec:
  source: {path: %q}
  signatures:
    - signer: %q
`, app, verified.Signer))
	options := Options{ConfigPath: project, CacheDir: b.TempDir(), Method: "update"}
	if _, err := Run(b.Context(), options); err != nil {
		b.Fatal(err)
	}
	for _, method := range []string{"update", "cold", "warm"} {
		b.Run(method, func(b *testing.B) {
			options := options
			options.Method = "prepare"
			if method == "update" {
				options.Method = "update"
			}
			if method == "warm" {
				if _, err := Run(b.Context(), options); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			if method == "cold" {
				options.CacheDir = filepath.Join(b.TempDir(), "cache")
			}
			for b.Loop() {
				if method == "cold" {
					b.StopTimer()
					if err := os.RemoveAll(options.CacheDir); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
				if _, err := Run(b.Context(), options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
