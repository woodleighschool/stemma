package diskimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
)

// Image is an open, read-only filesystem in a DMG. Files remain valid until Close.
// The source and the selected payload are never mounted or executed.
type Image struct {
	filesystem

	source      io.Closer
	closeVolume func()
}

// Open reads DMG metadata and opens its single filesystem. Data chunks are
// decompressed by filesystem reads, without creating a raw filesystem image.
func Open(ctx context.Context, filename string) (*Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	info, err := source.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("disk image must be a regular file")
	}
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	image, err := openImage(ctx, source, info.Size())
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("disk image: %w", err)
	}
	image.source = source
	return image, nil
}

func openImage(ctx context.Context, source io.ReaderAt, size int64) (*Image, error) {
	if size < 512 || size > maxBytes {
		return nil, errors.New("invalid disk image size")
	}
	dmg, err := disk.NewDMGReader(&imageReader{ctx: ctx, reader: source, remaining: maxBytes * 4}, size, disk.DMGLimits{MetadataBytes: 16 << 20, ChunkBytes: 64 << 20, ImageBytes: uint64(maxBytes)})
	if err != nil {
		return nil, err
	}
	partitions := dmg.Partitions()
	if len(partitions) > 256 {
		return nil, errors.New("disk image exceeds partition limit")
	}
	count := 0
	for _, p := range partitions {
		if strings.Contains(p.Name, "Apple_HFS") || strings.Contains(p.Name, "Apple_APFS") || p.Name == "disk image" || p.Name == "(disk image)" {
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("disk image has %d filesystems; exactly one is required", count)
	}
	if dmg.Size() <= 0 || dmg.Size() > maxBytes {
		return nil, errors.New("invalid filesystem size")
	}
	reader := &imageReader{ctx: ctx, reader: dmg, remaining: 256 << 20}
	volume, closeVolume, err := openVolume(reader, dmg.Size())
	if err != nil {
		return nil, err
	}
	reader.remaining = maxBytes * 4
	return &Image{filesystem: volume, closeVolume: closeVolume}, nil
}

// ReadLink returns a symlink's target, making an image an [fs.ReadLinkFS].
func (image *Image) ReadLink(name string) (string, error) {
	return image.Readlink(name)
}

// Lstat describes the named entry. Volumes never follow symlinks, so it is Stat.
func (image *Image) Lstat(name string) (fs.FileInfo, error) {
	return image.Stat(name)
}

// Select resolves an exact path or pattern, or discovers one unambiguous app/PKG.
func (image *Image) Select(ctx context.Context, selection string) (string, error) {
	return selectPayload(ctx, image, selection)
}

// Close releases the volume and source file.
func (image *Image) Close() error {
	image.closeVolume()
	if image.source != nil {
		return image.source.Close()
	}
	return nil
}
