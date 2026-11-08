package intunewin

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkValidateSource(b *testing.B) {
	source := b.TempDir()
	data := make([]byte, 1<<20)
	random := rand.NewChaCha8([32]byte{})
	if _, err := random.Read(data); err != nil {
		b.Fatal(err)
	}
	for i := range 16 {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("file-%02d.exe", i)), data, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := ValidateSource(b.Context(), source, "file-00.exe"); err != nil {
			b.Fatal(err)
		}
	}
}
