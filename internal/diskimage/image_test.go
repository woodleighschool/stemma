package diskimage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/apfswrite"
	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
)

type observedReader struct {
	io.ReaderAt

	reads [][2]int64
}

func (r *observedReader) ReadAt(p []byte, offset int64) (int, error) {
	r.reads = append(r.reads, [2]int64{offset, offset + int64(len(p))})
	return r.ReaderAt.ReadAt(p, offset)
}

func TestImageReadsSelectedChunks(t *testing.T) {
	for _, format := range []string{"HFS+", "HFSX", "APFS"} {
		t.Run(format, func(t *testing.T) {
			raw, err := os.Create(filepath.Join(t.TempDir(), "raw"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			const size = 32 << 20
			if err := raw.Truncate(size); err != nil {
				t.Fatal(err)
			}
			plist := []byte("small metadata")
			unrelated := bytes.Repeat([]byte("unrelated"), (16<<20)/9)
			if format == "APFS" {
				root := &apfswrite.Entry{Mode: fs.ModeDir | 0755, Children: []*apfswrite.Entry{
					{Name: "Foo.app", Mode: fs.ModeDir | 0755, Children: []*apfswrite.Entry{{Name: "Contents", Mode: fs.ModeDir | 0755, Children: []*apfswrite.Entry{{Name: "Info.plist", Mode: 0644, Data: plist}}}}},
					{Name: "Unrelated.bin", Mode: 0644, Data: unrelated},
				}}
				err = apfswrite.CreateContainer(raw, size, &apfswrite.CreateOptions{Root: root})
			} else {
				root := &hfsplus.Entry{Mode: fs.ModeDir | 0755, Children: []*hfsplus.Entry{
					{Name: "Foo.app", Mode: fs.ModeDir | 0755, Children: []*hfsplus.Entry{{Name: "Contents", Mode: fs.ModeDir | 0755, Children: []*hfsplus.Entry{{Name: "Info.plist", Mode: 0644, Data: plist}}}}},
					{Name: "Unrelated.bin", Mode: 0644, Data: unrelated},
				}}
				err = hfsplus.CreateImage(raw, size, "Fixture", root, &hfsplus.CreateOptions{CaseInsensitive: format == "HFS+"})
			}
			if err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(t.TempDir(), "image.dmg")
			kind := "Apple_HFS"
			if format == "APFS" {
				kind = "Apple_APFS"
			}
			if err := disk.WrapRawImageDMGFrom(filename, raw, size, kind, &disk.EncodeOptions{ChunkSectors: 128}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := disk.NewDMGReader(bytes.NewReader(data), int64(len(data)), nil)
			if err != nil {
				t.Fatal(err)
			}
			partitions := metadata.Partitions()
			if err := metadata.Close(); err != nil {
				t.Fatal(err)
			}
			assertSelective := func(t *testing.T, source *observedReader) {
				t.Helper()
				var total, touched uint64
				for _, part := range partitions {
					for _, chunk := range part.Chunks {
						if chunk.CompressedLength == 0 {
							continue
						}
						total += chunk.DiskLength
						for _, read := range source.reads {
							if read[0] < int64(chunk.CompressedOffset+chunk.CompressedLength) && read[1] > int64(chunk.CompressedOffset) {
								touched += chunk.DiskLength
								break
							}
						}
					}
				}
				if touched == 0 || touched >= total/4 {
					t.Fatalf("read chunks covering %d of %d expanded bytes", touched, total)
				}
			}
			for _, operation := range []string{"discover", "plist", "extract"} {
				t.Run(operation, func(t *testing.T) {
					source := &observedReader{ReaderAt: bytes.NewReader(data)}
					image, err := openImage(t.Context(), source, int64(len(data)))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = image.Close() }()
					selected, err := image.Select(t.Context(), "")
					if err != nil || selected != "Foo.app" {
						t.Fatalf("selection %q: %v", selected, err)
					}
					if operation == "plist" {
						got, err := fs.ReadFile(image, "Foo.app/Contents/Info.plist")
						if err != nil || !bytes.Equal(got, plist) {
							t.Fatalf("metadata %q: %v", got, err)
						}
					}
					if operation == "extract" {
						destination := filepath.Join(t.TempDir(), "payload")
						selected, err := image.Extract(t.Context(), destination, "", nil)
						if err != nil {
							t.Fatal(err)
						}
						got, err := os.ReadFile(filepath.Join(selected, "Contents/Info.plist"))
						if err != nil || !bytes.Equal(got, plist) {
							t.Fatalf("extracted metadata %q: %v", got, err)
						}
						if _, err := os.Stat(filepath.Join(destination, "Unrelated.bin")); !os.IsNotExist(err) {
							t.Fatal("extracted unrelated file")
						}
					}
					assertSelective(t, source)
				})
			}
		})
	}
}

func TestImageLZFSE(t *testing.T) {
	image := fixture(t, &hfsplus.Entry{Name: "Fixture.pkg", Mode: 0644, Data: []byte("xar!payload")})
	data, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	source, err := disk.NewDMGReader(bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	compressed := filepath.Join(t.TempDir(), "lzfse.dmg")
	if err := disk.WrapRawImageDMGFrom(compressed, source, source.Size(), "Apple_HFSX", &disk.EncodeOptions{Compression: disk.CompressionLZFSE}); err != nil {
		t.Fatal(err)
	}
	selected, err := Extract(t.Context(), compressed, filepath.Join(t.TempDir(), "payload"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(selected)
	if err != nil || string(got) != "xar!payload" {
		t.Fatalf("LZFSE bytes %q: %v", got, err)
	}
}

func TestImageReadBudgetAndCancellation(t *testing.T) {
	r := &imageReader{ctx: t.Context(), reader: bytes.NewReader([]byte("data")), remaining: 2}
	if _, err := r.ReadAt(make([]byte, 3), 0); err == nil {
		t.Fatal("accepted read beyond budget")
	}
	ctx, cancel := context.WithCancel(t.Context())
	filename := fixture(t, &hfsplus.Entry{Name: "Fixture.pkg", Mode: 0644, Data: []byte("xar!data")})
	image, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	cancel()
	if _, err := image.Select(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("selection after cancellation: %v", err)
	}
}
