// Package diskimage reads selected installer payloads from portable HFS+ and APFS DMGs.
package diskimage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/deploymenttheory/go-apfs-v2/pkg/apfs"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/text/unicode/norm"
)

const maxBytes int64 = 16 << 30
const maxEntries = 100000

// Extract writes the selected app or flat PKG under a new destination directory,
// returning its path. An empty selection requires one unambiguous payload.
// Partial output is removed on failure. Images are read without mounting them.
//
// The result is an inspection copy: file bytes, modes, times and confined symlinks.
// Filesystem metadata remains in the original DMG, which is the installer artifact.
// This copy must not be used to repackage an application.
func Extract(ctx context.Context, input, destination, selection string) (result string, err error) {
	image, err := Open(ctx, input)
	if err != nil {
		return "", err
	}
	defer func() { _ = image.Close() }()
	return image.Extract(ctx, destination, selection)
}

// Extract copies a selected payload from the open image into a new directory.
func (image *Image) Extract(ctx context.Context, destination, selection string) (result string, err error) {
	selection, err = image.Select(ctx, selection)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			done := plugin.Stage(ctx, "Removing incomplete extraction")
			done(os.RemoveAll(destination))
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := extract(ctx, image, root, selection); err != nil {
		return "", fmt.Errorf("disk image: %w", err)
	}
	return filepath.Join(destination, filepath.FromSlash(selection)), nil
}

type filesystem interface {
	fs.StatFS
	fs.ReadDirFS
	Readlink(string) (string, error)
}

func openVolume(reader io.ReaderAt, size int64) (filesystem, func(), error) {
	var header [48]byte
	if _, err := reader.ReadAt(header[:], 0); err != nil {
		return nil, nil, err
	}
	if string(header[32:36]) != "NXSB" {
		if err := validateVolume(reader, size); err != nil {
			return nil, nil, err
		}
		volume, err := hfsplus.New(reader)
		return volume, func() {}, err
	}
	blockSize := binary.LittleEndian.Uint32(header[36:40])
	blocks := binary.LittleEndian.Uint64(header[40:48])
	if blockSize < 4096 || blockSize > 65536 || blockSize&(blockSize-1) != 0 || blocks == 0 || blocks > uint64(size)/uint64(blockSize) {
		return nil, nil, errors.New("invalid APFS container size")
	}
	handle, err := apfs.NewIOHandle()
	if err != nil {
		return nil, nil, err
	}
	container, err := apfs.NewContainer(handle)
	if err != nil {
		return nil, nil, err
	}
	closeVolume := func() { _ = container.Close(); _ = handle.Close() }
	if err := container.OpenRead(reader, 0); err != nil {
		closeVolume()
		return nil, nil, err
	}
	count, err := container.NumberOfVolumes()
	if err != nil {
		closeVolume()
		return nil, nil, err
	}
	if count != 1 {
		closeVolume()
		return nil, nil, fmt.Errorf("APFS requires one readable volume (found %d)", count)
	}
	volume, err := container.Volume(0)
	if err != nil {
		closeVolume()
		return nil, nil, err
	}
	return volume, closeVolume, nil
}

type imageReader struct {
	mu        sync.Mutex
	ctx       context.Context
	reader    io.ReaderAt
	remaining int64
}

func (r *imageReader) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	if int64(len(p)) > r.remaining {
		r.mu.Unlock()
		return 0, errors.New("filesystem read limit exceeded")
	}
	r.remaining -= int64(len(p))
	r.mu.Unlock()
	return r.reader.ReadAt(p, offset)
}

func validateVolume(r io.ReaderAt, size int64) error {
	var header hfsplus.VolumeHeader
	if err := binary.Read(io.NewSectionReader(r, 1024, 512), binary.BigEndian, &header); err != nil {
		return err
	}
	if header.Signature != hfsplus.HFSPlusSigWord && header.Signature != hfsplus.HFSXSigWord {
		return errors.New("only HFS+ and HFSX disk image filesystems are supported")
	}
	if header.BlockSize < 512 || header.BlockSize > 65536 || header.BlockSize&(header.BlockSize-1) != 0 || uint64(header.TotalBlocks)*uint64(header.BlockSize) > uint64(size) {
		return errors.New("invalid HFS+ volume size")
	}
	if uint64(header.FileCount)+uint64(header.FolderCount) > maxEntries {
		return errors.New("filesystem exceeds entry limit")
	}
	for _, fork := range []hfsplus.ForkData{header.CatalogFile, header.ExtentsFile, header.AttributesFile} {
		if fork.LogicalSize > 64<<20 {
			return errors.New("filesystem metadata exceeds size limit")
		}
	}
	return nil
}

func safeName(name string) error {
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("unsafe disk image path %q", name)
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.TrimRight(part, " .") != part {
			return fmt.Errorf("unportable disk image path %q", name)
		}
	}
	return nil
}

func selectPayload(ctx context.Context, volume filesystem, selection string) (string, error) {
	if selection == "" {
		var candidates []string
		count := 0
		err := fs.WalkDir(volume, ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			count++
			if count > maxEntries {
				return errors.New("filesystem exceeds entry limit")
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			ext := strings.ToLower(path.Ext(name))
			if ext == ".app" && entry.IsDir() {
				candidates = append(candidates, name)
				return fs.SkipDir
			}
			if ext == ".pkg" && entry.Type().IsRegular() {
				candidates = append(candidates, name)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if len(candidates) != 1 {
			return "", fmt.Errorf("disk image has %d plausible payloads; set select to an exact relative path: %s", len(candidates), strings.Join(candidates, ", "))
		}
		selection = candidates[0]
	}
	var err error
	selection, err = archive.MatchPath(volume, selection)
	if err != nil {
		return "", err
	}
	if err := safeName(selection); err != nil {
		return "", err
	}
	for prefix := selection; prefix != "."; prefix = path.Dir(prefix) {
		info, err := volume.Stat(prefix)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", errors.New("selected payload must not traverse a symlink")
		}
		if prefix != selection && !info.IsDir() {
			return "", errors.New("selected payload parent is not a directory")
		}
	}
	info, err := volume.Stat(selection)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(path.Ext(selection)) {
	case ".app":
		if info.IsDir() {
			return selection, nil
		}
	case ".pkg":
		if info.Mode().IsRegular() {
			return selection, nil
		}
	}
	return "", errors.New("disk image selection must name an app bundle or flat PKG")
}

type entry struct {
	name string
	info fs.FileInfo
	link string
}

func extract(ctx context.Context, volume filesystem, root *os.Root, selection string) (err error) {
	var dirs, links []entry
	spelling := map[string]string{}
	var total int64
	count := 0
	buffer := make([]byte, 32<<10)
	lastProgress := time.Now()
	flatPackage := strings.EqualFold(path.Ext(selection), ".pkg")
	finalizing := false
	defer func() {
		if err != nil && finalizing {
			// Restore searchable parents if finalizing original modes failed.
			sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].name) < len(dirs[j].name) })
			for _, dir := range dirs {
				_ = root.Chmod(dir.name, 0o700)
			}
		}
	}()
	// A directory handle owns its children's writes. Reusing it avoids walking
	// every ancestor again for each create, chmod and timestamp update.
	var walk func(string, fs.FileInfo, *os.Root) error
	walk = func(name string, info fs.FileInfo, parent *os.Root) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := safeName(name); err != nil {
			return err
		}
		count++
		if time.Since(lastProgress) >= 250*time.Millisecond {
			plugin.Logger(ctx).InfoContext(ctx, "Extracting files", "progress", true, "current", count, "unit", "entries")
			lastProgress = time.Now()
		}
		if count > maxEntries {
			return errors.New("payload exceeds entry limit")
		}
		for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
			folded := strings.ToLower(norm.NFD.String(prefix))
			if prior, exists := spelling[folded]; exists && prior != prefix {
				return fmt.Errorf("case-conflicting paths %q and %q", prior, prefix)
			}
			spelling[folded] = prefix
		}
		if info.Size() < 0 || info.Size() > maxBytes-total {
			return errors.New("payload exceeds expanded size limit")
		}
		total += info.Size()
		base := path.Base(name)
		switch {
		case info.IsDir():
			if err := parent.Mkdir(base, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, entry{name: name, info: info})
			directory, err := parent.OpenRoot(base)
			if err != nil {
				return err
			}
			defer func() { _ = directory.Close() }()
			children, err := volume.ReadDir(name)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := safeName(child.Name()); err != nil || path.Base(child.Name()) != child.Name() {
					return fmt.Errorf("invalid directory entry %q", child.Name())
				}
				childInfo, err := child.Info()
				if err != nil {
					return err
				}
				if err := walk(path.Join(name, child.Name()), childInfo, directory); err != nil {
					return err
				}
			}
		case info.Mode()&fs.ModeSymlink != 0:
			if info.Size() > 4096 {
				return errors.New("symlink target exceeds size limit")
			}
			target, err := volume.Readlink(name)
			if err != nil {
				return err
			}
			resolved := path.Join(path.Dir(name), target)
			if target == "" || strings.ContainsAny(target, "\\:\x00") || path.IsAbs(target) || resolved != selection && !strings.HasPrefix(resolved, selection+"/") {
				return fmt.Errorf("escaping symlink %q", name)
			}
			links = append(links, entry{name: name, info: info, link: target})
		case info.Mode().IsRegular():
			return writeFile(ctx, volume, parent, name, info, flatPackage, buffer)
		default:
			return fmt.Errorf("unsupported file type in %q", name)
		}
		return nil
	}
	info, err := volume.Stat(selection)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(path.Dir(selection), 0o700); err != nil {
		return err
	}
	parent, err := root.OpenRoot(path.Dir(selection))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if err := walk(selection, info, parent); err != nil {
		return err
	}
	// No payload write is permitted to traverse a payload-owned symlink.
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Symlink(link.link, link.name); err != nil {
			return err
		}
	}
	finalizing = true
	for _, dir := range slices.Backward(dirs) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Chtimes(dir.name, dir.info.ModTime(), dir.info.ModTime()); err != nil {
			return err
		}
		if err := root.Chmod(dir.name, dir.info.Mode().Perm()); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func writeFile(ctx context.Context, volume filesystem, root *os.Root, name string, info fs.FileInfo, flatPackage bool, buffer []byte) error {
	source, err := volume.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	var content io.Reader = source
	if flatPackage {
		var magic [4]byte
		if _, err := io.ReadFull(source, magic[:]); err != nil {
			return err
		}
		if string(magic[:]) != "xar!" {
			return errors.New("selected PKG is not a flat XAR package")
		}
		content = io.MultiReader(strings.NewReader("xar!"), source)
	}
	out, err := root.OpenFile(path.Base(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// Hide ReadFrom so CopyBuffer reuses the extraction buffer for every file.
	n, err := io.CopyBuffer(struct{ io.Writer }{out}, io.LimitReader(fileio.Reader{Context: ctx, Reader: content}, info.Size()+1), buffer)
	if err == nil && n != info.Size() {
		err = fmt.Errorf("file length mismatch for %q", name)
	}
	if err == nil {
		err = out.Chmod(info.Mode().Perm())
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Chtimes(path.Base(name), info.ModTime(), info.ModTime())
}
