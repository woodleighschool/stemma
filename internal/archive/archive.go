// Package archive extracts a bounded portable subset of ZIP and TAR into leased workspaces.
package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mholt/archives"
	"github.com/woodleighschool/stemma/internal/fileio"
)

const maxBytes int64 = 16 << 30
const maxEntries = 100000

// Extract writes a new directory and removes partial outputs on failure.
// Device nodes, hard links and escaping paths are rejected.
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
		if appleDouble(entry.NameInArchive) {
			return nil
		}
		if header, ok := entry.Header.(*tar.Header); ok {
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

// appleDouble reports the sidecar entries macOS archivers add for extended
// attributes and resource forks, which extraction leaves behind.
func appleDouble(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		if part == "__MACOSX" || strings.HasPrefix(part, "._") {
			return true
		}
	}
	return false
}

// safeName accepts any relative POSIX path, because a macOS bundle carries
// names with colons, backslashes and trailing spaces. Names a host cannot
// represent are rejected by hostName, where content is written to disk.
func safeName(name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if name == "." {
		return name, nil
	}
	if !fs.ValidPath(name) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return name, nil
}

// hostName reports the local path for a safe archive name, and refuses names
// this filesystem cannot hold rather than writing something else.
func hostName(name string) (string, error) {
	local := filepath.FromSlash(name)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("archive path %q cannot be created on this host", name)
	}
	return local, nil
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
	if _, err := hostName(name); err != nil {
		return err
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
		if target == "" || strings.ContainsRune(target, 0) || path.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, "../") {
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
		name, err = MatchPath(r.FS(), name)
		if err != nil {
			return "", err
		}
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
