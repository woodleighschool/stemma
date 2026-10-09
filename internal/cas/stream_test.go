package cas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAuthenticatesUnreadTail(t *testing.T) {
	for _, mutation := range []string{"none", "same size", "truncated", "extended"} {
		t.Run(mutation, func(t *testing.T) {
			s, _ := testStore(t)
			ref, err := s.Import(t.Context(), strings.NewReader("complete content"), "")
			if err != nil {
				t.Fatal(err)
			}
			path, err := s.Path(ref)
			if err != nil {
				t.Fatal(err)
			}
			if mutation != "none" {
				data := map[string]string{"same size": "corrupt! content", "truncated": "complete", "extended": "complete content extra"}[mutation]
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = s.Read(t.Context(), ref, func(r io.Reader) error { _, err := io.ReadFull(r, make([]byte, 2)); return err })
			if (err == nil) != (mutation == "none") {
				t.Fatalf("read: %v", err)
			}
			target := filepath.Join(t.TempDir(), "copy")
			err = s.Materialize(t.Context(), ref, target)
			if mutation == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("private change"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := s.Verify(t.Context(), ref); err != nil {
					t.Fatal("copy shared cache storage:", err)
				}
				if err := s.Materialize(t.Context(), ref, target); err == nil {
					t.Fatal("overwrote target")
				}
			} else {
				if err == nil {
					t.Fatal("materialized corrupt bytes")
				}
				entries, err := os.ReadDir(filepath.Dir(target))
				if err != nil || len(entries) != 0 {
					t.Fatalf("partial output: %v %v", entries, err)
				}
			}
		})
	}
}

func TestWriteFailureNeverPublishes(t *testing.T) {
	for _, failure := range []string{"producer", "digest", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := testStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			expected := ""
			if failure == "digest" {
				expected = strings.Repeat("0", 64)
			}
			_, err := s.Write(ctx, expected, func(w io.Writer) error {
				if _, err := io.WriteString(w, "partial"); err != nil {
					return err
				}
				if failure == "producer" {
					return errors.New("producer failed")
				}
				if failure == "canceled" {
					cancel()
				}
				return nil
			})
			if err == nil {
				t.Fatal("accepted failed write")
			}
			entries, err := os.ReadDir(filepath.Join(s.Dir, "objects"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("published partial object: %v %v", entries, err)
			}
		})
	}
}

func TestDigestAndImportAgree(t *testing.T) {
	s, _ := testStore(t)
	write := func(w io.Writer) error { _, err := io.WriteString(w, "canonical content"); return err }
	digest, err := Digest(t.Context(), write)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ref, err := s.Write(t.Context(), digest.SHA256, write)
		if err != nil || ref != digest {
			t.Fatalf("import = %+v: %v", ref, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, "objects"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("objects = %v: %v", entries, err)
	}
	var output bytes.Buffer
	w := boundedWriter{writer: &output, size: MaxObjectSize - 1}
	if _, err := w.Write([]byte("too large")); err == nil || output.Len() != 0 {
		t.Fatal("oversized write reached output")
	}
}

func TestReadCancellationAfterConsumingBufferedContent(t *testing.T) {
	s, _ := testStore(t)
	ref, err := s.Import(t.Context(), strings.NewReader("small buffered object"), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = s.Read(ctx, ref, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		cancel()
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancellation: %v", err)
	}
}
