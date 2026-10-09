package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
)

func TestCanonicalTarRejectsSidecarSignatures(t *testing.T) {
	data, err := appledouble.FromXattrs(map[string][]byte{"com.apple.cs.CodeDirectory": []byte("signature")}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	w := tar.NewWriter(&packed)
	if err := w.WriteHeader(&tar.Header{Name: "._helper", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := IndexTar(t.Context(), bytes.NewReader(packed.Bytes()), int64(packed.Len()), "root", 0o755); err == nil || !strings.Contains(err.Error(), "keeps its code signature") {
		t.Fatalf("discarded sidecar signature: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "tree")
	if err := ExtractTar(t.Context(), bytes.NewReader(packed.Bytes()), destination); err == nil || !strings.Contains(err.Error(), "keeps its code signature") {
		t.Fatalf("materialized sidecar signature: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("retained rejected tree: %v", err)
	}
}

func TestCanonicalTarFilesystem(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "Versions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Versions", "binary"), []byte("executable"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("Versions/binary", filepath.Join(dir, "current")); err != nil {
		t.Skip(err)
	}
	var data bytes.Buffer
	if err := Pack(t.Context(), dir, &data); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	indexed, err := IndexTar(ctx, bytes.NewReader(data.Bytes()), int64(data.Len()), "App.app", 0o750)
	if err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(indexed, "App.app", "App.app/Versions", "App.app/Versions/binary", "App.app/current"); err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(indexed, "App.app")
	if err != nil {
		t.Fatal(err)
	}
	file, err := sub.Open("current")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	buf := make([]byte, 4)
	if n, err := file.(io.ReaderAt).ReadAt(buf, 2); err != nil || n != 4 || string(buf) != "ecut" {
		t.Fatalf("ReaderAt: %q %d %v", buf, n, err)
	}
	if n, err := file.(io.ReaderAt).ReadAt(buf, 8); !errors.Is(err, io.EOF) || n != 2 {
		t.Fatalf("read crossed entry: %d %v", n, err)
	}
	info, err := indexed.Lstat("App.app")
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("root mode: %v %v", info, err)
	}
	for _, name := range []string{"../App.app", "/App.app", "App.app/../App.app"} {
		if _, err := indexed.Open(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	cancel()
	if _, err := file.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestCanonicalTarRejectsUnrepresentableEntries(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"escape", []*tar.Header{{Name: "../file", Typeflag: tar.TypeReg}}},
		{"link escape", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside"}}},
		{"hardlink", []*tar.Header{{Name: "link", Typeflag: tar.TypeLink, Linkname: "file"}}},
		{"missing parent", []*tar.Header{{Name: "missing/file", Typeflag: tar.TypeReg}}},
		{"duplicate", []*tar.Header{{Name: "file", Typeflag: tar.TypeReg}, {Name: "file", Typeflag: tar.TypeReg}}},
		{"case collision", []*tar.Header{{Name: "File", Typeflag: tar.TypeReg}, {Name: "file", Typeflag: tar.TypeReg}}},
		{"unicode collision", []*tar.Header{{Name: "café", Typeflag: tar.TypeReg}, {Name: "cafe\u0301", Typeflag: tar.TypeReg}}},
		{"device", []*tar.Header{{Name: "device", Typeflag: tar.TypeChar}}},
		{"link parent", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "."}, {Name: "link/file", Typeflag: tar.TypeReg}}},
		{"privileged mode", []*tar.Header{{Name: "file", Typeflag: tar.TypeReg, Mode: 0o4755}}},
		{"xattr", []*tar.Header{{Name: "file", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"SCHILY.xattr.com.apple.cs.CodeDirectory": "signature"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var data bytes.Buffer
			writer := tar.NewWriter(&data)
			for _, header := range test.headers {
				if err := writer.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := IndexTar(t.Context(), bytes.NewReader(data.Bytes()), int64(data.Len()), "root", 0o755); err == nil {
				t.Fatal("accepted invalid tree")
			}
		})
	}
}

func TestCanonicalTarPreservesLiteralFilesInPhysicalLeases(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "__MACOSX"), 0o750); err != nil {
		t.Fatal(err)
	}
	sidecar, err := appledouble.FromXattrs(map[string][]byte{"com.apple.FinderInfo": make([]byte, 32)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"._literal": []byte("ordinary file"), "__MACOSX/data": []byte("ordinary directory"), "._data": sidecar,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	var packed bytes.Buffer
	if err := Pack(t.Context(), directory, &packed); err != nil {
		t.Fatal(err)
	}
	indexed, err := IndexTar(t.Context(), bytes.NewReader(packed.Bytes()), int64(packed.Len()), "root", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(t.TempDir(), "tree")
	if err := ExtractTar(t.Context(), bytes.NewReader(packed.Bytes()), physical); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"._literal", "__MACOSX/data", "._data"} {
		want, err := fs.ReadFile(indexed, "root/"+name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(physical, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("physical lease changed %s: %v", name, err)
		}
	}
	var roundtrip bytes.Buffer
	if err := Pack(t.Context(), physical, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packed.Bytes(), roundtrip.Bytes()) {
		t.Fatal("physical lease changed canonical identity")
	}
}

func TestCanonicalTarConfinesSymlinkResolution(t *testing.T) {
	var packed bytes.Buffer
	w := tar.NewWriter(&packed)
	for _, header := range []*tar.Header{
		{Name: "outside", Typeflag: tar.TypeReg},
		{Name: "dir", Typeflag: tar.TypeDir},
		{Name: "dir/alias", Typeflag: tar.TypeSymlink, Linkname: "../outside"},
		{Name: "dir/chain", Typeflag: tar.TypeSymlink, Linkname: "alias"},
		{Name: "dir/loop", Typeflag: tar.TypeSymlink, Linkname: "loop"},
	} {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	indexed, err := IndexTar(t.Context(), bytes.NewReader(packed.Bytes()), int64(packed.Len()), "root", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(indexed, "root/dir")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alias", "chain", "loop"} {
		if _, err := sub.Open(name); err == nil {
			t.Fatalf("followed escaping or cyclic link %s", name)
		}
		if _, err := fs.ReadLink(sub, name); err != nil {
			t.Fatalf("cannot retain link %s: %v", name, err)
		}
	}
}

func TestCanonicalTarResolvesSymlinksBeforeParentComponents(t *testing.T) {
	var packed bytes.Buffer
	w := tar.NewWriter(&packed)
	for _, header := range []*tar.Header{
		{Name: "dir", Typeflag: tar.TypeDir},
		{Name: "dir/deep", Typeflag: tar.TypeDir},
		{Name: "dir/deep/more", Typeflag: tar.TypeDir},
		{Name: "dir/deep/file", Typeflag: tar.TypeReg},
		{Name: "dir/alias", Typeflag: tar.TypeSymlink, Linkname: "deep/more"},
		{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "alias/../file"},
	} {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	indexed, err := IndexTar(t.Context(), bytes.NewReader(packed.Bytes()), int64(packed.Len()), "root", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(indexed, "root/dir/link"); err != nil {
		t.Fatalf("collapsed parent component before resolving alias: %v", err)
	}
}

func TestCanonicalTarLongNamesAndTerminators(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	name := strings.Repeat("name", 40)
	if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 3, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("end")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"none", "truncate payload", "missing terminator", "trailing data"} {
		t.Run(change, func(t *testing.T) {
			value := bytes.Clone(data.Bytes())
			switch change {
			case "truncate payload":
				value = value[:1538]
			case "missing terminator":
				value = value[:len(value)-1024]
			case "trailing data":
				value = append(value, make([]byte, 512)...)
			}
			indexed, err := IndexTar(t.Context(), bytes.NewReader(value), int64(len(value)), "root", 0o755)
			if (err == nil) != (change == "none") {
				t.Fatalf("index: %v", err)
			}
			if err == nil {
				content, err := fs.ReadFile(indexed, "root/"+name)
				if err != nil || string(content) != "end" {
					t.Fatalf("content: %q %v", content, err)
				}
			}
		})
	}
}
