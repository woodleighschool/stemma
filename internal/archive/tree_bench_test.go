package archive

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// STEMMA_BENCH_APP selects a read-only real bundle; otherwise the benchmark
// builds 4,096 small files at two depths to expose ancestor-resolution costs.
func BenchmarkPack(b *testing.B) {
	if app := os.Getenv("STEMMA_BENCH_APP"); app != "" {
		b.Run("installed", func(b *testing.B) { benchmarkPack(b, app) })
		return
	}
	for _, depth := range []int{1, 12} {
		b.Run(fmt.Sprintf("depth%d", depth), func(b *testing.B) {
			root := b.TempDir()
			for i := range 64 {
				dir := filepath.Join(root, fmt.Sprintf("%03d", i), strings.Repeat("nested/", depth-1))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					b.Fatal(err)
				}
				for j := range 64 {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d", j)), []byte("synthetic resource"), 0o600); err != nil {
						b.Fatal(err)
					}
				}
			}
			benchmarkPack(b, root)
		})
	}
}

func benchmarkPack(b *testing.B, root string) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := Pack(b.Context(), root, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
