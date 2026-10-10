package diskimage

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/testutil/testbundle"
	"github.com/woodleighschool/stemma/internal/treefs"
)

func BenchmarkDiskImage(b *testing.B) {
	app := testbundle.Write(b)
	root, err := os.OpenRoot(filepath.Dir(app))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	source := treefs.Local(root)
	for _, compression := range []Compression{LZFSE, Zlib, LZMA} {
		b.Run(string(compression), func(b *testing.B) {
			filename := filepath.Join(b.TempDir(), "Benchmark.dmg")
			write := func(b *testing.B, output string) {
				b.Helper()
				if err := WriteApplication(b.Context(), source, "Benchmark.app", output, compression, time.Unix(1700000000, 0)); err != nil {
					b.Fatal(err)
				}
			}
			write(b, filename)
			b.Run("open", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					image, err := Open(b.Context(), filename)
					if err != nil {
						b.Fatal(err)
					}
					if err := image.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("read", func(b *testing.B) {
				b.SetBytes(17 << 20)
				b.ReportAllocs()
				for b.Loop() {
					image, err := Open(b.Context(), filename)
					if err != nil {
						b.Fatal(err)
					}
					file, err := image.Open("Benchmark.app/Contents/MacOS/benchmark")
					if err != nil {
						_ = image.Close()
						b.Fatal(err)
					}
					n, err := io.Copy(io.Discard, file)
					_ = file.Close()
					closeErr := image.Close()
					if err != nil || closeErr != nil || n != 17<<20 {
						b.Fatalf("read %d bytes: %v; close: %v", n, err, closeErr)
					}
				}
			})
			b.Run("build", func(b *testing.B) {
				output := filepath.Join(b.TempDir(), "Benchmark.dmg")
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					if err := os.RemoveAll(output); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					write(b, output)
				}
			})
		})
	}
}
