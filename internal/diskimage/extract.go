// Package diskimage reads selected installer payloads from portable HFS+ DMGs.
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

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/deploymenttheory/go-apfs-v2/pkg/hfsplus"
	"github.com/woodleighschool/stemma/internal/fileio"
	"golang.org/x/text/unicode/norm"
)

const maxBytes int64 = 16 << 30
const maxEntries = 100000

// Extract writes the selected app or flat PKG under a new destination directory,
// returning its path. An empty selection requires one unambiguous payload.
// Partial output is removed on failure. Images are read without mounting them.
//
// Flat PKGs are self-contained XAR data forks; their enclosing volume's Finder
// metadata is not installer content. App trees retain modes, times and relative
// symlinks, and reject metadata the portable package representation cannot carry.
func Extract(ctx context.Context, input, destination, selection string) (result string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateDMG(ctx, input); err != nil {
		return "", fmt.Errorf("disk image: %w", err)
	}
	image, err := disk.OpenDMG(input)
	if err != nil {
		return "", fmt.Errorf("disk image: %w", err)
	}
	defer func() { _ = image.Close() }()
	if image.Size() <= 0 || image.Size() > maxBytes {
		return "", errors.New("disk image filesystem exceeds size limit")
	}
	reader := &imageReader{ctx: ctx, reader: image, remaining: 256 << 20}
	if err := validateVolume(reader, image.Size()); err != nil {
		return "", fmt.Errorf("disk image: %w", err)
	}
	volume, err := hfsplus.New(reader)
	if err != nil {
		return "", fmt.Errorf("disk image: %w", err)
	}
	reader.remaining = maxBytes * 4
	selection, err = selectPayload(ctx, volume, selection)
	if err != nil {
		return "", fmt.Errorf("disk image: %w", err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := extract(ctx, volume, root, selection); err != nil {
		return "", fmt.Errorf("disk image: %w", err)
	}
	return filepath.Join(destination, filepath.FromSlash(selection)), nil
}

type imageReader struct {
	ctx       context.Context
	reader    io.ReaderAt
	remaining int64
}

func (r *imageReader) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		return 0, errors.New("filesystem read limit exceeded")
	}
	r.remaining -= int64(len(p))
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

func selectPayload(ctx context.Context, volume *hfsplus.Volume, selection string) (string, error) {
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
			return "", fmt.Errorf("disk image has %d plausible payloads; set select to an exact relative path", len(candidates))
		}
		selection = candidates[0]
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

func extract(ctx context.Context, volume *hfsplus.Volume, root *os.Root, selection string) (err error) {
	var dirs, links []entry
	spelling := map[string]string{}
	fileIDs := map[hfsplus.CatalogNodeID]bool{}
	var total int64
	count := 0
	flatPackage := strings.EqualFold(path.Ext(selection), ".pkg")
	defer func() {
		if err != nil {
			// Restore searchable parents if finalizing original modes failed.
			sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].name) < len(dirs[j].name) })
			for _, dir := range dirs {
				_ = root.Chmod(dir.name, 0o700)
			}
		}
	}()
	err = fs.WalkDir(volume, selection, func(name string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := safeName(name); err != nil {
			return err
		}
		count++
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
		info, err := item.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > maxBytes-total {
			return errors.New("payload exceeds expanded size limit")
		}
		total += info.Size()
		if !flatPackage {
			if err := checkMetadata(volume, name, info, fileIDs); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := root.Mkdir(name, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, entry{name: name, info: info})
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
			return writeFile(ctx, volume, root, name, info, flatPackage)
		default:
			return fmt.Errorf("unsupported file type in %q", name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// No payload write is permitted to traverse a payload-owned symlink.
	for _, link := range links {
		if err := root.Symlink(link.link, link.name); err != nil {
			return err
		}
	}
	for _, dir := range slices.Backward(dirs) {
		if err := root.Chtimes(dir.name, dir.info.ModTime(), dir.info.ModTime()); err != nil {
			return err
		}
		if err := root.Chmod(dir.name, dir.info.Mode().Perm()); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func writeFile(ctx context.Context, volume *hfsplus.Volume, root *os.Root, name string, info fs.FileInfo, flatPackage bool) error {
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
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(fileio.Reader{Context: ctx, Reader: content}, info.Size()+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if n != info.Size() {
		return fmt.Errorf("file length mismatch for %q", name)
	}
	if err := root.Chmod(name, info.Mode().Perm()); err != nil {
		return err
	}
	return root.Chtimes(name, info.ModTime(), info.ModTime())
}

func checkMetadata(volume *hfsplus.Volume, name string, info fs.FileInfo, fileIDs map[hfsplus.CatalogNodeID]bool) error {
	var permissions hfsplus.BSDInfo
	switch record := info.Sys().(type) {
	case *hfsplus.HFSPlusCatalogFile:
		permissions = record.BSDInfo
		if fileIDs[record.FileID] || permissions.Special > 1 {
			return errors.New("hard links are unsupported")
		}
		fileIDs[record.FileID] = true
		// HFS+ encodes ordinary symbolic links with the slnk/rhap Finder pair.
		linkType := info.Mode()&fs.ModeSymlink != 0 && record.UserInfo.FileType == hfsplus.SymLinkFileType && record.UserInfo.FileCreator == hfsplus.SymLinkCreator
		if !linkType && (record.UserInfo.FileType != 0 || record.UserInfo.FileCreator != 0) || record.UserInfo.FinderFlags != 0 {
			return errors.New("finder file metadata is unsupported")
		}
	case *hfsplus.HFSPlusCatalogFolder:
		permissions = record.BSDInfo
		if record.UserInfo.FinderFlags != 0 {
			return errors.New("finder folder metadata is unsupported")
		}
	default:
		return errors.New("missing filesystem metadata")
	}
	if permissions.FileMode&0o7000 != 0 || permissions.OwnerFlags != 0 || permissions.AdminFlags != 0 {
		return errors.New("special permissions and filesystem flags are unsupported")
	}
	attrs, err := volume.Xattrs(name)
	if err != nil {
		return err
	}
	if len(attrs) != 0 {
		return errors.New("extended attributes and resource forks are unsupported in app payloads")
	}
	return nil
}
