package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

// BenchmarkPrepareCatalog builds independent packages from one shared input.
// Cold clears Stemma's cache between runs, not the OS filesystem cache.
func BenchmarkPrepareCatalog(b *testing.B) {
	root := b.TempDir()
	data := make([]byte, 8<<20)
	random := rand.NewChaCha8([32]byte{})
	if _, err := random.Read(data); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), data, 0o644); err != nil {
		b.Fatal(err)
	}
	var manifest strings.Builder
	manifest.WriteString("apiVersion: stemma/v1alpha1\nkind: Project\nmetadata: {name: benchmark}\nspec:\n  imports: ['*.software.yaml']\n")
	for i := range 6 {
		fmt.Fprintf(&manifest, `---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata: {name: package-%d}
spec:
  inputs:
    data: {path: payload}
  payload:
    /Library/Example/data:
      $input: data
  package:
    identifier: org.example.package-%d
    version: '1.0'
`, i, i)
	}
	project := filepath.Join(root, "stemma.yaml")
	testproject.Write(b, project, manifest.String())
	options := Options{ConfigPath: project, CacheDir: b.TempDir(), Method: "update"}
	if _, err := Run(b.Context(), options); err != nil {
		b.Fatal(err)
	}
	for _, jobs := range []int{1, 2, 4} {
		for _, warm := range []bool{false, true} {
			b.Run(fmt.Sprintf("jobs=%d/warm=%t", jobs, warm), func(b *testing.B) {
				options := options
				options.Method, options.Jobs = "prepare", jobs
				options.CacheDir = filepath.Join(b.TempDir(), "cache")
				if warm {
					if _, err := Run(b.Context(), options); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportAllocs()
				for b.Loop() {
					if !warm {
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
}

func BenchmarkPrepareUnchangedInput(b *testing.B) {
	store, err := cas.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	ref, err := store.Import(b.Context(), bytes.NewReader(make([]byte, 16<<20)), "")
	if err != nil {
		b.Fatal(err)
	}
	ops := &operations{registry: plugin.New("fixture", "1")}
	if err := plugin.Register(ops.registry, resourceOperation("fixture", "Fixture"), func(_ context.Context, request plugin.ResourceRequest[struct{}]) (plugin.ResourceResult, error) {
		return plugin.ResourceResult{Artifacts: request.Inputs}, nil
	}); err != nil {
		b.Fatal(err)
	}
	inputs := map[string]Prepared{"installer": {Payload: ref, Filename: "installer.pkg", Mode: 0o644}}
	plan := resourcePlan{Operation: "fixture", Resource: config.Resource{APIVersion: "example.test/v1", Kind: "Fixture", Metadata: config.Metadata{Name: "installer"}}}
	work := filepath.Join(b.TempDir(), "work")
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := prepareResource(b.Context(), store, ops, plan, inputs, work, "signature"); err != nil {
			b.Fatal(err)
		}
		if err := os.RemoveAll(work); err != nil {
			b.Fatal(err)
		}
	}
}

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
