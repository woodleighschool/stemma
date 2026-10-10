package apple

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/signature"
)

// STEMMA_BENCH_APP selects an installed bundle instead of the fixture. It verifies the
// complete bundle on each iteration and never alters it or caches evidence.
func BenchmarkVerifyApp(b *testing.B) {
	app := os.Getenv("STEMMA_BENCH_APP")
	if app == "" {
		app = "testdata/NestedFixture.app"
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := VerifyApp(b.Context(), app, signature.Signer{}); err != nil {
			b.Fatal(err)
		}
	}
}

// Acquisition is excluded: this measures verification against the same
// canonical tree representation supplied during preparation.
func BenchmarkVerifyPackedApp(b *testing.B) {
	app := os.Getenv("STEMMA_BENCH_APP")
	if app == "" {
		app = "testdata/NestedFixture.app"
	}
	root, err := os.Stat(app)
	if err != nil {
		b.Fatal(err)
	}
	f, err := os.Create(filepath.Join(b.TempDir(), "tree.tar"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := archive.Pack(b.Context(), app, f); err != nil {
		b.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		b.Fatal(err)
	}
	name := filepath.Base(app)
	tree, err := archive.IndexTar(b.Context(), f, info.Size(), name, root.Mode().Perm())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := VerifyAppFS(b.Context(), tree, name, signature.Signer{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerifyPackage(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := VerifyPackage(b.Context(), "testdata/fixture.pkg", signature.Signer{}); err != nil {
			b.Fatal(err)
		}
	}
}
