package pkgbuild

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/testutil/testbundle"
)

func BenchmarkPackage(b *testing.B) {
	app := testbundle.Write(b)
	for _, compression := range []Compression{Gzip, XZ} {
		b.Run(string(compression), func(b *testing.B) {
			opts := Options{Identifier: "org.example.benchmark", Version: "1.0", Payload: ".", InstallLocation: "/Applications/Benchmark.app", Compression: compression, Timestamp: time.Unix(1700000000, 0)}
			output := filepath.Join(b.TempDir(), "Benchmark.pkg")
			if err := Build(b.Context(), app, output, opts); err != nil {
				b.Fatal(err)
			}
			b.Run("inspect", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					facts, err := apple.InspectPackageContents(b.Context(), output)
					if err != nil || len(facts.Applications) != 1 {
						b.Fatalf("inspect: %v, applications: %d", err, len(facts.Applications))
					}
				}
			})
			b.Run("extract", func(b *testing.B) {
				stage := filepath.Join(b.TempDir(), "expanded")
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					if err := os.RemoveAll(stage); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					if _, err := apple.ExtractApplication(b.Context(), output, "Payload", opts.InstallLocation, stage, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("build", func(b *testing.B) {
				filename := filepath.Join(b.TempDir(), "Benchmark.pkg")
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					if err := os.RemoveAll(filename); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					if err := Build(b.Context(), app, filename, opts); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func BenchmarkPackageVaried(b *testing.B) {
	app := testbundle.Write(b)
	data := make([]byte, 17<<20)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(data)
	// Mix compressible pages with distinct deterministic bytes across both PBZX blocks.
	for offset := 0; offset < len(data); offset += 4096 {
		clear(data[offset : offset+2048])
	}
	if err := os.WriteFile(filepath.Join(app, "Contents/MacOS/benchmark"), data, 0o755); err != nil {
		b.Fatal(err)
	}
	opts := Options{Identifier: "org.example.benchmark", Version: "1.0", Payload: ".", InstallLocation: "/Applications/Benchmark.app", Compression: XZ, Timestamp: time.Unix(1700000000, 0)}
	output := filepath.Join(b.TempDir(), "Benchmark.pkg")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		if err := os.RemoveAll(output); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := Build(b.Context(), app, output, opts); err != nil {
			b.Fatal(err)
		}
	}
}
