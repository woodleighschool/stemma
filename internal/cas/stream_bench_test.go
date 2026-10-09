package cas

import (
	"io"
	"testing"
)

// Small writes and reads model canonical TAR headers and small bundle files.
func BenchmarkObjectStream(b *testing.B) {
	store, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	block := make([]byte, 512)
	const blocks = 8192
	write := func(w io.Writer) error {
		for range blocks {
			if _, err := w.Write(block); err != nil {
				return err
			}
		}
		return nil
	}
	ref, err := store.Write(b.Context(), "", write)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("write", func(b *testing.B) {
		b.SetBytes(ref.Size)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := store.Write(b.Context(), ref.SHA256, write); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("read", func(b *testing.B) {
		b.SetBytes(ref.Size)
		b.ReportAllocs()
		for b.Loop() {
			err := store.Read(b.Context(), ref, func(r io.Reader) error {
				for range blocks {
					if _, err := io.ReadFull(r, block); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
