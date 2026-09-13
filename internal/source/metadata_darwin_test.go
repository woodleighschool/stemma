package source

import (
	"archive/tar"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/sys/unix"
)

func TestNativeFileArtifactMetadata(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "download metadata"
		if compressed {
			name = "filesystem compression"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			filename := filepath.Join(root, "input.pkg")
			payload := bytes.Repeat([]byte("synthetic package content\n"), 128)
			if err := os.WriteFile(filename, payload, 0o640); err != nil {
				t.Fatal(err)
			}
			if compressed {
				var data bytes.Buffer
				w := zlib.NewWriter(&data)
				if _, err := w.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				header := binary.LittleEndian.AppendUint32(nil, 0x636d7066)
				header = binary.LittleEndian.AppendUint32(header, 3)
				header = binary.LittleEndian.AppendUint64(header, uint64(len(payload)))
				if err := os.Truncate(filename, 0); err != nil {
					t.Fatal(err)
				}
				if err := unix.Setxattr(filename, "com.apple.decmpfs", append(header, data.Bytes()...), 0); err != nil {
					t.Fatal(err)
				}
				if err := unix.Chflags(filename, unix.UF_COMPRESSED); err != nil {
					t.Fatal(err)
				}
			} else {
				// A materialized file can retain a visible decmpfs attribute.
				header := binary.LittleEndian.AppendUint32(nil, 0x636d7066)
				header = binary.LittleEndian.AppendUint32(header, 0x80000001)
				header = binary.LittleEndian.AppendUint64(header, uint64(len(payload)))
				if err := unix.Setxattr(filename, "com.apple.decmpfs", header, 0); err != nil {
					t.Fatal(err)
				}
				if err := unix.Chflags(filename, unix.UF_TRACKED); err != nil {
					t.Fatal(err)
				}
			}
			if err := unix.Setxattr(filename, "com.apple.quarantine", []byte("0081;00000000;Fixture;"), 0); err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(filename, "com.apple.metadata:kMDItemWhereFroms", []byte(`<plist version="1.0"><array><string>https://example.invalid/input.pkg</string></array></plist>`), 0); err != nil {
				t.Fatal(err)
			}
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New(store, root, false)
			input := plugin.Input{Resolver: "file", Config: map[string]any{"path": "input.pkg"}}
			entry, err := m.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if entry.Content.Mode != 0o640 || entry.Content.Artifact.Size != int64(len(payload)) {
				t.Fatalf("file mode or logical size changed: %+v", entry.Content)
			}
			cached, err := store.Path(entry.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{filename, cached} {
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, payload) {
					t.Fatalf("source or cached bytes changed: %v", err)
				}
			}
			artifact := plugin.Artifact{Path: filename, Filename: "input.pkg", SHA256: entry.Content.Artifact.SHA256, Size: int64(len(payload))}
			content, err := m.importArtifact(t.Context(), artifact)
			if err != nil || content != entry.Content {
				t.Fatalf("resolver artifact differs from native file import: %+v: %v", content, err)
			}
			if err := unix.Setxattr(filename, "com.apple.quarantine", []byte("0081;00000001;Fixture;"), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := m.FetchLocked(t.Context(), input, entry); err != nil {
				t.Fatalf("host metadata invalidated the lock: %v", err)
			}
			tree, err := m.Resolve(t.Context(), plugin.Input{Resolver: "local", Config: map[string]any{"include": []string{"input.pkg"}}})
			if err != nil {
				t.Fatal(err)
			}
			treePath, err := store.Path(tree.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(treePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			reader := tar.NewReader(file)
			header, err := reader.Next()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			if err != nil || header.Name != "input.pkg" || header.Mode != 0o640 || !bytes.Equal(data, payload) {
				t.Fatalf("tree import changed file content or mode: %v", err)
			}
			if err := os.WriteFile(filename, []byte("changed content"), 0o640); err != nil {
				t.Fatal(err)
			}
			if _, err := m.FetchLocked(t.Context(), input, entry); err == nil {
				t.Fatal("changed bytes retained the lock")
			}
		})
	}
}
