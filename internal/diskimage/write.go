package diskimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

// Compression names the codec that compresses a disk image's data chunks.
type Compression string

// Chunk codecs, as hdiutil's ULFO, UDZO and ULMO formats use them.
const (
	LZFSE Compression = "lzfse"
	Zlib  Compression = "zlib"
	LZMA  Compression = "lzma"
)

func (c Compression) codec() (disk.Compression, error) {
	switch c {
	case LZFSE:
		return disk.CompressionLZFSE, nil
	case Zlib:
		return disk.CompressionZlib, nil
	case LZMA:
		return disk.CompressionLZMA, nil
	}
	return 0, fmt.Errorf("unsupported disk image compression %q", c)
}

// WriteApplication writes a new DMG holding one application bundle at the root
// of an HFS+ volume named after it, with its chunks compressed by compression.
// The image carries the bundle's bytes, attributes, permission bits and confined
// relative symlinks, owned by root, with every date set to timestamp, so the same bundle,
// compression and timestamp produce the same bytes on every host. The bundle is
// never mounted or executed.
// Source names and symlink targets use POSIX paths, independent of the host.
func WriteApplication(ctx context.Context, source fs.ReadLinkFS, app, output string, compression Compression, timestamp time.Time) (err error) {
	done := plugin.Stage(ctx, "Building disk image", plugin.Detail(filepath.Base(output)))
	defer func() { done(err) }()
	codec, err := compression.codec()
	if err != nil {
		return err
	}
	if err := safeName(app); err != nil {
		return err
	}
	name := path.Base(app)
	if !strings.EqualFold(filepath.Ext(name), ".app") {
		return errors.New("disk image requires an application bundle")
	}
	// HFS+ dates count unsigned 32-bit seconds from 1904.
	if seconds := timestamp.Unix() - hfsEpoch; seconds < 0 || seconds > math.MaxUint32 {
		return errors.New("disk image timestamp must be representable as an HFS+ date")
	}
	if _, err := os.Lstat(output); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("disk image output already exists or cannot be checked")
	}
	for parent := app; parent != "."; parent = path.Dir(parent) {
		info, err := source.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("application path traverses a symlink or nondirectory")
		}
	}
	info, err := source.Lstat(app)
	if err != nil {
		return err
	}
	w := writer{ctx: ctx, source: source, selection: app, timestamp: timestamp}
	bundle, err := w.entry(app, name, info)
	if err != nil {
		return fmt.Errorf("disk image: %w", err)
	}
	volume, err := os.CreateTemp(filepath.Dir(output), ".volume-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = volume.Close()
		_ = os.Remove(volume.Name())
	}()
	root := &hfsplus.Entry{Mode: fs.ModeDir | 0o755, Children: []*hfsplus.Entry{bundle}}
	label := strings.TrimSuffix(name, filepath.Ext(name))
	if err := hfsplus.CreateImage(volume, 0, label, root, &hfsplus.CreateOptions{FixedTime: timestamp, CaseInsensitive: true}); err != nil {
		return fmt.Errorf("disk image: %w", err)
	}
	size, err := volume.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if err := disk.WrapRawImageDMGFrom(output, contextReaderAt{ctx, volume}, size, "Apple_HFS", &disk.EncodeOptions{Compression: codec}); err != nil {
		return fmt.Errorf("disk image: %w", err)
	}
	return ctx.Err()
}

// hfsEpoch is 1904-01-01T00:00:00Z in Unix seconds.
const hfsEpoch = -2082844800

type writer struct {
	ctx       context.Context
	source    fs.ReadLinkFS
	selection string
	timestamp time.Time
	entries   int
	total     int64
}

// entry describes the file at name, relative to the bundle, under its stored name.
func (w *writer) entry(name, stored string, info fs.FileInfo) (*hfsplus.Entry, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	if w.entries++; w.entries > maxEntries {
		return nil, errors.New("application exceeds entry limit")
	}
	if err := safeName(stored); err != nil {
		return nil, err
	}
	if err := archive.CheckMode(info); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	// Open supplies logical bytes; retained compression attributes must not select storage.
	entry := &hfsplus.Entry{Name: stored, ModTime: w.timestamp, ModeExplicit: true, BSDFlags: new(uint32)}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := w.source.ReadLink(name)
		if err != nil {
			return nil, err
		}
		resolved := path.Join(path.Dir(name), target)
		if target == "" || strings.ContainsRune(target, 0) || path.IsAbs(target) || resolved != w.selection && !strings.HasPrefix(resolved, w.selection+"/") {
			return nil, fmt.Errorf("escaping symlink %s", name)
		}
		// Hosts disagree on a symlink's own permission bits; macOS writes these.
		entry.Mode, entry.Data = fs.ModeSymlink|0o755, []byte(target)
	case info.IsDir():
		entry.Mode = fs.ModeDir | info.Mode().Perm()
		children, err := w.children(name)
		if err != nil {
			return nil, err
		}
		entry.Children = children
	case info.Mode().IsRegular():
		if info.Size() < 0 || info.Size() > maxBytes-w.total {
			return nil, errors.New("application exceeds size limit")
		}
		w.total += info.Size()
		entry.Mode, entry.Size = info.Mode().Perm(), info.Size()
		entry.Open = func() (io.ReadCloser, error) { return w.open(name) }
	default:
		return nil, fmt.Errorf("unsupported file type in %q", name)
	}
	if metadata, ok := w.source.(interface {
		XattrValues(string) (map[string]appledouble.Value, error)
	}); ok {
		values, err := metadata.XattrValues(name)
		if err != nil {
			return nil, err
		}
		for key, value := range values {
			if value == nil || value.Size() < 0 || value.Size() > maxBytes-w.total {
				return nil, errors.New("application metadata exceeds size limit")
			}
			w.total += value.Size()
			value = io.NewSectionReader(contextReaderAt{w.ctx, value}, 0, value.Size())
			if key == appledouble.ResourceForkName {
				entry.ResourceForkValue = value
			} else {
				if entry.XattrValues == nil {
					entry.XattrValues = map[string]appledouble.Value{}
				}
				entry.XattrValues[key] = value
			}
		}
	}
	return entry, nil
}

func (w *writer) children(dir string) ([]*hfsplus.Entry, error) {
	directory, err := w.source.Open(dir)
	if err != nil {
		return nil, err
	}
	dirFile, ok := directory.(fs.ReadDirFile)
	if !ok {
		_ = directory.Close()
		return nil, errors.New("application directory cannot be read")
	}
	listing, err := dirFile.ReadDir(maxEntries - w.entries + 1)
	_ = directory.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	children := make([]*hfsplus.Entry, 0, len(listing))
	for _, child := range listing {
		name := path.Join(dir, child.Name())
		info, err := w.source.Lstat(name)
		if err != nil {
			return nil, err
		}
		entry, err := w.entry(name, child.Name(), info)
		if err != nil {
			return nil, err
		}
		children = append(children, entry)
	}
	return children, nil
}

// open yields a file's content while the volume is written, so no more than the
// volume writer's copy buffer is held at once.
func (w *writer) open(name string) (io.ReadCloser, error) {
	file, err := w.source.Open(name)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{fileio.Reader{Context: w.ctx, Reader: file}, file}, nil
}

type contextReaderAt struct {
	ctx    context.Context
	reader io.ReaderAt
}

func (r contextReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.ReadAt(p, offset)
}
