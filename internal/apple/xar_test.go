package apple

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/signature"
)

func TestXARRequiresChecksumsForStoredData(t *testing.T) {
	checksums := func(stored, decoded []byte) string {
		return fmt.Sprintf(`<archived-checksum style="sha256">%x</archived-checksum><extracted-checksum style="sha256">%x</extracted-checksum>`, sha256.Sum256(stored), sha256.Sum256(decoded))
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressedEmpty := compressed.Bytes()
	for _, tt := range []struct {
		name   string
		data   string
		heap   []byte
		accept bool
	}{
		{"absent", "", nil, true},
		{"zero size", `<size>0</size>`, nil, true},
		{"zero descriptor", `<size>0</size><length>0</length><offset>32</offset><encoding style="application/octet-stream"/>`, nil, true},
		{"empty checksums", `<offset>32</offset>` + checksums(nil, nil), nil, true},
		{"empty compressed", fmt.Sprintf(`<offset>32</offset><length>%d</length><size>0</size><encoding style="application/x-gzip"/>`, len(compressedEmpty)) + checksums(compressedEmpty, nil), compressedEmpty, true},
		{"content", `<offset>32</offset><length>1</length><size>1</size>` + checksums([]byte("a"), []byte("a")), []byte("a"), true},
		{"content missing checksums", `<offset>32</offset><length>1</length><size>1</size>`, []byte("a"), false},
		{"stored bytes with zero size", `<offset>32</offset><length>1</length>`, []byte("a"), false},
		{"decoded size without stored bytes", `<size>1</size>`, nil, false},
		{"missing length with checksums", `<offset>32</offset><size>1</size>` + checksums([]byte("a"), []byte("a")), []byte("a"), false},
		{"missing size with checksums", `<offset>32</offset><length>1</length>` + checksums([]byte("a"), []byte("a")), []byte("a"), false},
		{"negative offset", `<offset>-1</offset>`, nil, false},
		{"outside heap", `<offset>33</offset>`, nil, false},
		{"unknown encoding", `<encoding style="unknown"/>`, nil, false},
		{"missing compressed stream", `<encoding style="application/x-gzip"/>`, nil, false},
		{"one checksum", `<archived-checksum style="sha256">00</archived-checksum>`, nil, false},
		{"blank checksums", `<archived-checksum style="sha256"/><extracted-checksum style="sha256"/>`, nil, false},
		{"wrong empty digests", checksums([]byte("a"), []byte("a")), nil, false},
	} {
		for _, attribute := range []bool{false, true} {
			kind := "file"
			if attribute {
				kind = "attribute"
			}
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				contents := ""
				if attribute {
					contents = `<ea id="0"><name>user.fixture</name>` + tt.data + `</ea>`
				} else if tt.name != "absent" {
					contents = `<data>` + tt.data + `</data>`
				}
				name := writeXARContents(t, contents, tt.heap)
				// These archives have a valid TOC digest but no signature. Reaching
				// ErrUnsigned proves content validation succeeded, not signer trust.
				_, err := VerifyPackage(t.Context(), name, signature.Signer{})
				if errors.Is(err, signature.ErrUnsigned) != tt.accept || err == nil {
					t.Fatalf("VerifyPackage: %v; want content accepted = %v", err, tt.accept)
				}
				if tt.accept && !attribute {
					data := readTestFile(t, name)
					archive, err := openXAR(bytes.NewReader(data), int64(len(data)))
					if err != nil {
						t.Fatal(err)
					}
					var decoded bytes.Buffer
					if err := archive.readEntry("fixture", &decoded, maxEntrySize); err != nil {
						t.Fatal(err)
					}
					want := ""
					if tt.name == "content" {
						want = "a"
					}
					if decoded.String() != want {
						t.Fatalf("decoded %q, want %q", decoded.String(), want)
					}
				}
			})
		}
	}
}

// Raw XML keeps absent elements distinct from explicit zeroes in the fixtures.
func writeXARContents(t *testing.T, contents string, heap []byte) string {
	t.Helper()
	toc := `<xar><toc><checksum style="sha256"><offset>0</offset><size>32</size></checksum><file id="1"><name>fixture</name><type>file</type>` + contents + `</file></toc></xar>`
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := io.Copy(zw, strings.NewReader(toc)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 28)
	copy(header, "xar!")
	binary.BigEndian.PutUint16(header[4:], 28)
	binary.BigEndian.PutUint16(header[6:], 1)
	binary.BigEndian.PutUint64(header[8:], uint64(compressed.Len()))
	binary.BigEndian.PutUint64(header[16:], uint64(len(toc)))
	binary.BigEndian.PutUint32(header[24:], 3)
	digest := sha256.Sum256(compressed.Bytes())
	data := header
	data = append(data, compressed.Bytes()...)
	data = append(data, digest[:]...)
	data = append(data, heap...)
	name := filepath.Join(t.TempDir(), "fixture.xar")
	writeTestFile(t, name, data, 0o600)
	return name
}

func TestNativeXAREmptyContents(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native xar is only available on macOS")
	}
	for _, contents := range []string{"", `<ea id="0"><name>user.fixture</name></ea>`, `<data><size>0</size><length>0</length><offset>32</offset></data>`} {
		name := writeXARContents(t, contents, nil)
		dir := t.TempDir()
		cmd := exec.CommandContext(t.Context(), "/usr/bin/xar", "-xf", name, "-C", dir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("native extraction: %v: %s", err, output)
		}
		if data := readTestFile(t, filepath.Join(dir, "fixture")); len(data) != 0 {
			t.Fatalf("expected empty file, got %d bytes", len(data))
		}
	}
}
