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
	"unicode/utf16"

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/text/unicode/norm"
)

// WriteApplication writes a new zlib-compressed DMG holding one application
// bundle at the root of an HFS+ volume named after it. The image carries the
// bundle's bytes, permission bits and confined relative symlinks, owned by root,
// with every date set to timestamp, so the same bundle and timestamp produce the
// same bytes on every host. The bundle is never mounted or executed.
func WriteApplication(ctx context.Context, app, output string, timestamp time.Time) (err error) {
	done := plugin.Stage(ctx, "Building disk image", plugin.Detail(filepath.Base(output)))
	defer func() { done(err) }()
	name := filepath.Base(app)
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
	source, err := os.OpenRoot(app)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	info, err := source.Lstat(".")
	if err != nil {
		return err
	}
	w := writer{ctx: ctx, source: source, timestamp: timestamp}
	bundle, err := w.entry(".", name, info)
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
	if err := disk.WrapRawImageDMGFrom(output, contextReaderAt{ctx, volume}, size, "Apple_HFS", nil); err != nil {
		return fmt.Errorf("disk image: %w", err)
	}
	return ctx.Err()
}

// hfsEpoch is 1904-01-01T00:00:00Z in Unix seconds.
const hfsEpoch = -2082844800

type writer struct {
	ctx       context.Context
	source    *os.Root
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
	if len(utf16.Encode([]rune(norm.NFD.String(stored)))) > 255 {
		return nil, fmt.Errorf("name %q exceeds the HFS+ limit", stored)
	}
	if err := archive.CheckMode(info); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	entry := &hfsplus.Entry{Name: stored, ModTime: w.timestamp}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := archive.Readlink(w.source, name)
		if err != nil {
			return nil, err
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
	return entry, nil
}

func (w *writer) children(dir string) ([]*hfsplus.Entry, error) {
	directory, err := w.source.Open(dir)
	if err != nil {
		return nil, err
	}
	listing, err := directory.ReadDir(maxEntries - w.entries + 1)
	_ = directory.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// The volume folds case, so names that differ only by case are one name.
	spelling := map[string]string{}
	children := make([]*hfsplus.Entry, 0, len(listing))
	for _, child := range listing {
		folded := strings.ToLower(norm.NFD.String(child.Name()))
		if prior, exists := spelling[folded]; exists {
			return nil, fmt.Errorf("case-conflicting paths %q and %q", path.Join(dir, prior), path.Join(dir, child.Name()))
		}
		spelling[folded] = child.Name()
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
