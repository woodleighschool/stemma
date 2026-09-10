package diskimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
	"howett.net/plist"
)

func fixture(t *testing.T, children ...*hfsplus.Entry) string {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "volume"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	const size = 8 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := hfsplus.CreateImage(f, size, "Fixture", &hfsplus.Entry{Mode: fs.ModeDir | 0o755, Children: children}, nil); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "fixture.dmg")
	if err := disk.WrapRawImageDMGFrom(name, f, size, "Apple_HFSX", nil); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestExtractApp(t *testing.T) {
	modified := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	image := fixture(t,
		&hfsplus.Entry{Name: "Applications", Mode: fs.ModeSymlink | 0o777, Data: []byte("/Applications")},
		&hfsplus.Entry{Name: ".background", Mode: 0o644, Data: []byte("not payload"), Xattrs: map[string][]byte{"example.display": []byte("presentation")}},
		&hfsplus.Entry{Name: "Fixture.app", Mode: fs.ModeDir | 0o755, Children: []*hfsplus.Entry{
			{Name: "Contents", Mode: fs.ModeDir | 0o755, Children: []*hfsplus.Entry{
				{Name: "executable", Mode: 0o751, ModTime: modified, Data: []byte("do not execute")},
				{Name: "Current", Mode: fs.ModeSymlink | 0o777, Data: []byte("executable")},
			}},
		}},
	)
	destination := filepath.Join(t.TempDir(), "extracted")
	selected, err := Extract(t.Context(), image, destination, "")
	if err != nil {
		t.Fatal(err)
	}
	if selected != filepath.Join(destination, "Fixture.app") {
		t.Fatalf("selected %q", selected)
	}
	data, err := os.ReadFile(filepath.Join(selected, "Contents", "executable"))
	if err != nil || string(data) != "do not execute" {
		t.Fatalf("content %q, error %v", data, err)
	}
	info, err := os.Stat(filepath.Join(selected, "Contents", "executable"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 || !info.ModTime().Equal(modified) {
		t.Fatalf("mode %v, modified %v", info.Mode(), info.ModTime())
	}
	link, err := os.Readlink(filepath.Join(selected, "Contents", "Current"))
	if err != nil || link != "executable" {
		t.Fatalf("link %q, error %v", link, err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %v, error %v", entries, err)
	}
}

func TestExtractFlatPackageDataFork(t *testing.T) {
	content := []byte("xar!opaque signed installer bytes")
	image := fixture(t, &hfsplus.Entry{Name: "Installers", Mode: fs.ModeDir | 0o755, Children: []*hfsplus.Entry{
		{Name: "Vendor.pkg", Mode: 0o644, Data: content, ResourceFork: []byte("Finder custom icon")},
	}})
	destination := filepath.Join(t.TempDir(), "extracted")
	selected, err := Extract(t.Context(), image, destination, "Installers/Vendor.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if selected != filepath.Join(destination, "Installers", "Vendor.pkg") {
		t.Fatalf("selected %q", selected)
	}
	got, err := os.ReadFile(selected)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("data %q, error %v", got, err)
	}
}

func TestExtractRejectsUnrepresentableApp(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []*hfsplus.Entry
		want    string
	}{
		{name: "xattr", entries: []*hfsplus.Entry{{Name: "file", Mode: 0o644, Data: []byte("x"), Xattrs: map[string][]byte{"example.required": []byte("metadata")}}}, want: "extended attributes"},
		{name: "resource fork", entries: []*hfsplus.Entry{{Name: "file", Mode: 0o644, Data: []byte("x"), ResourceFork: []byte("required")}}, want: "resource forks"},
		{name: "escaping link", entries: []*hfsplus.Entry{{Name: "link", Mode: fs.ModeSymlink | 0o777, Data: []byte("../outside")}}, want: "escaping symlink"},
		{name: "absolute link", entries: []*hfsplus.Entry{{Name: "link", Mode: fs.ModeSymlink | 0o777, Data: []byte("/etc/passwd")}}, want: "escaping symlink"},
		{name: "hard link", entries: []*hfsplus.Entry{{Name: "one", Mode: 0o644, Data: []byte("shared"), LinkGroup: 1}, {Name: "two", Mode: 0o644, Data: []byte("shared"), LinkGroup: 1}}, want: "hard links"},
		{name: "case conflict", entries: []*hfsplus.Entry{{Name: "File", Mode: 0o644, Data: []byte("one")}, {Name: "file", Mode: 0o644, Data: []byte("two")}}, want: "case-conflicting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := fixture(t, &hfsplus.Entry{Name: "Fixture.app", Mode: fs.ModeDir | 0o755, Children: test.entries})
			destination := filepath.Join(t.TempDir(), "extracted")
			_, err := Extract(t.Context(), image, destination, "")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("partial output remains: %v", err)
			}
		})
	}
}

func TestExtractSelection(t *testing.T) {
	image := fixture(t, &hfsplus.Entry{Name: "One.pkg", Mode: 0o644, Data: []byte("xar!one")}, &hfsplus.Entry{Name: "Two.pkg", Mode: 0o644, Data: []byte("xar!two")}, &hfsplus.Entry{Name: "Alias.pkg", Mode: fs.ModeSymlink | 0o777, Data: []byte("One.pkg")})
	for _, test := range []struct{ selection, want string }{{"", "2 plausible payloads"}, {"../One.pkg", "unsafe"}, {"Alias.pkg", "symlink"}, {"Missing.pkg", "not exist"}} {
		t.Run(test.selection, func(t *testing.T) {
			_, err := Extract(t.Context(), image, filepath.Join(t.TempDir(), "extracted"), test.selection)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
	if _, err := Extract(t.Context(), image, filepath.Join(t.TempDir(), "extracted"), "Two.pkg"); err != nil {
		t.Fatal(err)
	}
}

func TestExtractPreservesExistingDestinationAndCancellation(t *testing.T) {
	image := fixture(t, &hfsplus.Entry{Name: "Fixture.pkg", Mode: 0o644, Data: []byte("xar!fixture")})
	destination := t.TempDir()
	sentinel := filepath.Join(destination, "owned")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(t.Context(), image, destination, ""); err == nil {
		t.Fatal("accepted existing destination")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("existing output changed: %q %v", data, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Extract(ctx, image, filepath.Join(t.TempDir(), "extracted"), ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled extract: %v", err)
	}
}

func TestExtractRejectsMalformedDMG(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*disk.DMGFooter, []block)
		want   string
	}{
		{name: "large plist", mutate: func(f *disk.DMGFooter, _ []block) { f.PlistLength = 1 << 62 }, want: "footer exceeds"},
		{name: "large chunk", mutate: func(_ *disk.DMGFooter, b []block) { binary.BigEndian.PutUint64(b[0].Data[220:], 1<<62) }, want: "chunk exceeds"},
		{name: "truncated expansion", mutate: func(_ *disk.DMGFooter, b []block) { binary.BigEndian.PutUint64(b[0].Data[220:], 1) }, want: "length mismatch"},
		{name: "unknown codec", mutate: func(_ *disk.DMGFooter, b []block) { binary.BigEndian.PutUint32(b[0].Data[204:], 0x80000007) }, want: "unsupported disk image compression"},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := fixture(t, &hfsplus.Entry{Name: "Fixture.pkg", Mode: 0o644, Data: []byte("xar!fixture")})
			rewriteDMG(t, image, test.mutate)
			_, err := Extract(t.Context(), image, filepath.Join(t.TempDir(), "extracted"), "")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
}

func rewriteDMG(t *testing.T, filename string, mutate func(*disk.DMGFooter, []block)) {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var footer disk.DMGFooter
	if err := binary.Read(bytes.NewReader(data[len(data)-512:]), binary.BigEndian, &footer); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ResourceFork struct {
			Blocks []block `plist:"blkx"`
		} `plist:"resource-fork"`
	}
	if _, err := plist.Unmarshal(data[footer.PlistOffset:footer.PlistOffset+footer.PlistLength], &doc); err != nil {
		t.Fatal(err)
	}
	originalLength := footer.PlistLength
	mutate(&footer, doc.ResourceFork.Blocks)
	xml, err := plist.Marshal(doc, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if footer.PlistLength == originalLength {
		footer.PlistLength = uint64(len(xml))
	}
	output := bytes.NewBuffer(data[:footer.PlistOffset])
	output.Write(xml)
	if err := binary.Write(output, binary.BigEndian, &footer); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompressedChunkLengths(t *testing.T) {
	// Independent byte fixtures from ADC's literal encoding and Python's bz2.
	bz, err := hex.DecodeString("425a68393141592653593d1367fe00000e81802404c020200030c00451a69139352a54a913c5dc914e14240f44d9ff80")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		codec      uint32
		data, want []byte
	}{
		{"ADC", 0x80000004, append([]byte{0x86}, []byte("payload")...), []byte("payload")},
		{"bzip2", 0x80000006, bz, []byte(strings.Repeat("payload", 5))},
	} {
		t.Run(test.name, func(t *testing.T) {
			chunk := disk.DMGChunk{Type: test.codec, CompressedLength: uint64(len(test.data)), DiskLength: uint64(len(test.want))}
			var output bytes.Buffer
			if err := validateChunk(t.Context(), bytes.NewReader(test.data), chunk, &output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output.Bytes(), test.want) {
				t.Fatalf("decoded %q", output.Bytes())
			}
			chunk.DiskLength--
			if err := validateChunk(t.Context(), bytes.NewReader(test.data), chunk, io.Discard); err == nil {
				t.Fatal("accepted more decompressed bytes than declared")
			}
		})
	}
}

func TestGPTCountIsBoundedBeforeOpening(t *testing.T) {
	header := make([]byte, 512)
	copy(header, "EFI PART")
	binary.LittleEndian.PutUint32(header[12:], 92)
	binary.LittleEndian.PutUint32(header[80:], 0xffffffff)
	binary.LittleEndian.PutUint32(header[84:], 128)
	filename := filepath.Join(t.TempDir(), "malformed.dmg")
	blocks := []disk.SourceBlock{
		{Name: "GPT Header (Primary GPT Header : 1)", StartSector: 0, SectorCount: 1, Data: header},
		{Name: "GPT Partition Data (Primary GPT Table : 2)", StartSector: 1, SectorCount: 1, Data: make([]byte, 512)},
		{Name: "disk image (Apple_HFS : 3)", StartSector: 2, SectorCount: 1, Data: make([]byte, 512)},
	}
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.EncodeUDIF(f, blocks, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Extract(t.Context(), filename, filepath.Join(t.TempDir(), "extracted"), "")
	if err == nil || !strings.Contains(err.Error(), "GPT partition count") {
		t.Fatalf("error %v", err)
	}
}
