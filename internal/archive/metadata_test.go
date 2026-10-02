package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/base64"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
)

func metadataZIP(t *testing.T, names, data []string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "input.zip")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for i, name := range names {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, data[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestArchiveMetadataUnicodeOwner(t *testing.T) {
	encoded, err := appledouble.FromXattrs(map[string][]byte{"attribute": []byte("value")}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{{"caf\u00e9", "._cafe\u0301"}, {"cafe\u0301", "._caf\u00e9"}} {
		tree, err := Open(t.Context(), metadataZIP(t, names, []string{"payload", string(encoded)}), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		entries, err := fs.ReadDir(tree, ".")
		if err != nil || len(entries) != 1 {
			t.Fatalf("payload entries: %v: %v", entries, err)
		}
		attrs, err := tree.XattrValues(entries[0].Name())
		if err != nil || attrs["attribute"] == nil {
			t.Fatalf("normalized owner lost attributes: %v", err)
		}
		if err := tree.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchiveMetadataOwnership(t *testing.T) {
	encoded, err := appledouble.FromXattrs(map[string][]byte{
		"com.apple.cs.CodeSignature": []byte("signature"),
		appledouble.ResourceForkName: bytes.Repeat([]byte("fork"), 20000),
		"empty":                      {},
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		names, data []string
		rejection   string
	}{
		{"before owner", []string{"__MACOSX/._file", "file", "._content"}, []string{string(encoded), "payload", "ordinary file"}, ""},
		{"after owner", []string{"file", "._file"}, []string{"payload", string(encoded)}, ""},
		{"orphan", []string{"._missing"}, []string{string(encoded)}, "metadata owner"},
		{"owner case mismatch", []string{"file", "._File"}, []string{"payload", string(encoded)}, "no exact archive entry"},
		{"traversal", []string{"__MACOSX/../._file"}, []string{string(encoded)}, "unsafe archive path"},
		{"malformed", []string{"file", "._file"}, []string{"payload", string(encoded[:20])}, "AppleDouble sidecar"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			tree, err := Open(t.Context(), metadataZIP(t, test.names, test.data), workspace)
			if test.rejection != "" {
				if err == nil || !strings.Contains(err.Error(), test.rejection) {
					t.Fatalf("got %v, want %s", err, test.rejection)
				}
				entries, _ := os.ReadDir(workspace)
				if len(entries) != 0 {
					t.Fatal("failed extraction retained its workspace")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tree.Close() }()
			attrs, err := tree.XattrValues("file")
			if err != nil {
				t.Fatal(err)
			}
			if attrs["empty"] == nil || attrs["empty"].Size() != 0 {
				t.Fatal("empty attribute lost")
			}
			fork := attrs[appledouble.ResourceForkName]
			data, err := io.ReadAll(io.NewSectionReader(fork, 0, fork.Size()))
			if err != nil || !bytes.Equal(data, bytes.Repeat([]byte("fork"), 20000)) {
				t.Fatalf("resource fork changed: %v", err)
			}
			if test.name == "before owner" {
				data, err := os.ReadFile(filepath.Join(tree.Name(), "._content"))
				if err != nil || string(data) != "ordinary file" {
					t.Fatalf("ordinary dot-underscore file lost: %v", err)
				}
			}
			if err := tree.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := fork.ReadAt(make([]byte, 1), 0); err == nil {
				t.Fatal("metadata outlived its owner")
			}
		})
	}
}

func TestPAXMetadata(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		filename := filepath.Join(t.TempDir(), "input.tar")
		f, err := os.Create(filename)
		if err != nil {
			t.Fatal(err)
		}
		other := "attribute"
		if conflict {
			other = "different"
		}
		w := tar.NewWriter(f)
		if err := w.WriteHeader(&tar.Header{Name: "file", Mode: 0o644, Size: 1, Format: tar.FormatPAX, PAXRecords: map[string]string{
			"SCHILY.xattr.com.apple.cs.CodeSignature":     "attribute",
			"LIBARCHIVE.xattr.com.apple.cs.CodeSignature": base64.StdEncoding.EncodeToString([]byte(other)),
			"SCHILY.xattr.org.example.%name":              "value",
			"LIBARCHIVE.xattr.org.example.%25name":        base64.RawStdEncoding.EncodeToString([]byte("value")),
		}}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		tree, err := Open(t.Context(), filename, t.TempDir())
		if conflict {
			if err == nil || !strings.Contains(err.Error(), "conflicting archive attribute") {
				t.Fatalf("conflict accepted: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		attrs, err := tree.XattrValues("file")
		if err != nil {
			t.Fatal(err)
		}
		value := attrs["com.apple.cs.CodeSignature"]
		if attrs["org.example.%name"] == nil || attrs["org.example.%25name"] != nil {
			t.Fatal("encoded PAX attribute name changed")
		}
		data, err := io.ReadAll(io.NewSectionReader(value, 0, value.Size()))
		if err != nil || string(data) != "attribute" {
			t.Fatalf("PAX value changed: %v", err)
		}
		if err := tree.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
