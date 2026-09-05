// Package archive extracts a bounded portable subset of ZIP and TAR into leased workspaces.
package archive

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zip"
	"github.com/mholt/archives"
	"github.com/woodleighschool/stemma/internal/fileio"
)

const maxBytes int64 = 16 << 30
const maxEntries = 100000

// Extract writes a new directory and removes partial outputs on failure.
// Device nodes, hard links, extended attributes and escaping paths are rejected.
func Extract(ctx context.Context, input, destination string) (err error) {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	x := extractor{ctx: ctx, root: root, seen: map[string]bool{}, spelling: map[string]string{}}
	f, err := os.Open(input)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	format, stream, err := archives.Identify(ctx, input, f)
	if err != nil {
		return err
	}
	reader, ok := format.(archives.Extractor)
	if !ok {
		return fmt.Errorf("%s is not a supported archive", input)
	}
	err = reader.Extract(ctx, stream, func(_ context.Context, entry archives.FileInfo) error {
		if header, ok := entry.Header.(zip.FileHeader); ok {
			if err := zipMetadata(header); err != nil {
				return err
			}
		}
		if header, ok := entry.Header.(*tar.Header); ok {
			for key := range header.PAXRecords {
				if strings.HasPrefix(key, "SCHILY.xattr.") || strings.HasPrefix(key, "LIBARCHIVE.") {
					return fmt.Errorf("unsupported archive metadata %q", key)
				}
			}
			switch header.Typeflag {
			case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
			default:
				return fmt.Errorf("unsupported TAR entry type %d", header.Typeflag)
			}
		}
		if entry.Size() < 0 || entry.Size() > maxBytes {
			return errors.New("archive entry exceeds size limit")
		}
		if entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return x.write(entry.NameInArchive, entry.Mode(), entry.Size(), entry.LinkTarget, strings.NewReader(""))
		}
		data, err := entry.Open()
		if err != nil {
			return err
		}
		writeErr := x.write(entry.NameInArchive, entry.Mode(), entry.Size(), entry.LinkTarget, data)
		closeErr := data.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	return x.finish()
}

type link struct{ name, target string }
type directory struct {
	name string
	mode fs.FileMode
}
type extractor struct {
	ctx      context.Context
	root     *os.Root
	seen     map[string]bool
	spelling map[string]string
	total    int64
	links    []link
	dirs     []directory
}

func zipMetadata(header zip.FileHeader) error {
	for part := range strings.SplitSeq(header.Name, "/") {
		if part == "__MACOSX" || strings.HasPrefix(part, "._") {
			return fmt.Errorf("unsupported AppleDouble metadata in %q", header.Name)
		}
	}
	for extra := header.Extra; len(extra) > 0; {
		if len(extra) < 4 {
			return errors.New("truncated ZIP extra field")
		}
		kind := binary.LittleEndian.Uint16(extra)
		size := int(binary.LittleEndian.Uint16(extra[2:]))
		if size > len(extra)-4 {
			return errors.New("truncated ZIP extra field")
		}
		switch kind {
		case 0x0001, 0x000a, 0x5455, 0x5855, 0x7875, 0x7075, 0x6375:
			// ZIP64, timestamps, Unix ownership, and Unicode names/comments.
		default:
			return fmt.Errorf("unsupported ZIP metadata 0x%04x in %q", kind, header.Name)
		}
		extra = extra[4+size:]
	}
	return nil
}

func safeName(name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if name == "." {
		return name, nil
	}
	if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name || !filepath.IsLocal(filepath.FromSlash(name)) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.TrimRight(part, " .") != part {
			return "", fmt.Errorf("unportable archive path %q", name)
		}
	}
	return name, nil
}

func (x *extractor) write(name string, mode fs.FileMode, size int64, target string, data io.Reader) error {
	if err := x.ctx.Err(); err != nil {
		return err
	}
	name, err := safeName(name)
	if err != nil {
		return err
	}
	if name == "." && mode.IsDir() {
		return nil
	}
	if len(x.seen) >= maxEntries {
		return errors.New("archive exceeds entry limit")
	}
	folded := strings.ToLower(name)
	for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
		key := strings.ToLower(prefix)
		if prior, exists := x.spelling[key]; exists && prior != prefix {
			return fmt.Errorf("case-conflicting archive paths %q and %q", prior, prefix)
		}
		x.spelling[key] = prefix
	}
	if x.seen[folded] {
		return fmt.Errorf("duplicate or case-conflicting archive path %q", name)
	}
	x.seen[folded] = true
	if size < 0 || size > maxBytes-x.total {
		return errors.New("archive exceeds expanded size limit")
	}
	x.total += size
	if err := x.root.MkdirAll(path.Dir(name), 0o700); err != nil {
		return err
	}
	if mode.IsDir() {
		if err := x.root.MkdirAll(name, 0o700); err != nil {
			return err
		}
		x.dirs = append(x.dirs, directory{name, mode.Perm()})
		return nil
	}
	if mode&os.ModeSymlink != 0 {
		if target == "" {
			if size > 4096 {
				return errors.New("symlink target too large")
			}
			raw, err := io.ReadAll(io.LimitReader(data, 4097))
			if err != nil {
				return err
			}
			target = string(raw)
		}
		resolved := path.Join(path.Dir(name), target)
		if target == "" || strings.ContainsAny(target, "\\:\x00") || path.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, "../") {
			return fmt.Errorf("escaping symlink %q", name)
		}
		x.links = append(x.links, link{name, target})
		return nil
	}
	if !mode.IsRegular() {
		return fmt.Errorf("unsupported entry mode for %s", name)
	}
	f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(fileio.Reader{Context: x.ctx, Reader: data}, size+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("archive entry %s length mismatch", name)
	}
	return x.root.Chmod(name, mode.Perm())
}

func (x *extractor) finish() error {
	// Links are installed last so no earlier write can traverse an archive-owned link.
	for _, l := range x.links {
		if err := x.root.Symlink(l.target, l.name); err != nil {
			return err
		}
	}
	sort.Slice(x.dirs, func(i, j int) bool { return len(x.dirs[i].name) > len(x.dirs[j].name) })
	for _, d := range x.dirs {
		if err := x.root.Chmod(d.name, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// Select requires an explicit selection when an archive contains multiple plausible payloads.
func Select(root, selection string) (string, error) {
	if selection != "" {
		name, err := safeName(selection)
		if err != nil {
			return "", err
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			return "", err
		}
		defer func() { _ = r.Close() }()
		info, err := r.Lstat(name)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("selected payload must not be a symlink")
		}
		return filepath.Join(root, filepath.FromSlash(name)), nil
	}
	var candidates []string
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filename == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(filename))
		if ext == ".app" && entry.IsDir() {
			candidates = append(candidates, filename)
			return filepath.SkipDir
		}
		if !entry.IsDir() && (ext == ".pkg" || ext == ".msi" || ext == ".exe" || ext == ".dmg") {
			candidates = append(candidates, filename)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		entries, err := os.ReadDir(root)
		if err != nil {
			return "", err
		}
		if len(entries) == 1 && entries[0].Type()&os.ModeSymlink == 0 {
			return filepath.Join(root, entries[0].Name()), nil
		}
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("archive has %d plausible payloads; set select to an exact relative path", len(candidates))
	}
	return candidates[0], nil
}
