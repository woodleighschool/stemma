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

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/mholt/archives"
	"github.com/woodleighschool/stemma/internal/fileio"
	"golang.org/x/text/unicode/norm"
)

const maxBytes int64 = 16 << 30
const maxEntries = 100000

// IsArchive recognizes ZIP and TAR contents without relying on a download URL's
// filename. A format claimed by the name is still opened, so malformed archives
// fail extraction instead of silently becoming scalar inputs.
func IsArchive(ctx context.Context, name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	format, _, err := identify(ctx, name, f)
	if errors.Is(err, archives.NoMatch) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch format := format.(type) {
	case archives.Zip, archives.Tar:
		return true, nil
	case archives.CompressedArchive:
		_, tar := format.Extraction.(archives.Tar)
		return tar, nil
	}
	return false, nil
}

func identify(ctx context.Context, name string, f *os.File) (archives.Format, io.Reader, error) {
	format, _, err := archives.Identify(ctx, "", fileio.Reader{Context: ctx, Reader: io.LimitReader(f, 1<<20)})
	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		return nil, nil, seekErr
	}
	if errors.Is(err, archives.NoMatch) {
		// Empty TAR archives have no member header to identify by content.
		format, _, err = archives.Identify(ctx, name, nil)
	}
	return format, f, err
}

// Extract writes a new directory and removes partial outputs on failure.
// Device nodes, hard links and escaping paths are rejected.
func Extract(ctx context.Context, input, destination string) error {
	return extractArchive(ctx, input, destination, nil)
}

func extractArchive(ctx context.Context, input, destination string, metadata *Tree) (err error) {
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
	format, stream, err := identify(ctx, input, f)
	if err != nil {
		return err
	}
	reader, ok := format.(archives.Extractor)
	if !ok {
		return fmt.Errorf("%s is not a supported archive", input)
	}
	err = reader.Extract(ctx, stream, func(_ context.Context, entry archives.FileInfo) error {
		if metadata == nil && appleDouble(entry.NameInArchive) {
			return sidecarSignature(entry)
		}
		if header, ok := entry.Header.(*tar.Header); ok {
			if metadata != nil {
				if err := metadata.pax(ctx, entry.NameInArchive, header.PAXRecords); err != nil {
					return err
				}
			}
			for key := range header.PAXRecords {
				// GNU tar and libarchive name extended attributes in PAX records.
				if metadata == nil && (strings.HasPrefix(key, "SCHILY.xattr."+codeSignature) || strings.HasPrefix(key, "LIBARCHIVE.xattr."+codeSignature)) {
					return fmt.Errorf("%s keeps its code signature in extended attributes, which extraction would discard", entry.NameInArchive)
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
		if metadata != nil && entry.Size() > maxBytes-x.total-metadata.bytes {
			return errors.New("archive exceeds expanded size limit")
		}
		if metadata != nil && entry.IsDir() && (entry.NameInArchive == "__MACOSX/" || strings.HasPrefix(entry.NameInArchive, "__MACOSX/")) {
			return nil
		}
		if entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return x.write(entry.NameInArchive, entry.Mode(), entry.Size(), entry.LinkTarget, strings.NewReader(""))
		}
		data, err := entry.Open()
		if err != nil {
			return err
		}
		var writeErr error
		if metadata != nil && appledouble.IsSidecarName(entry.NameInArchive) {
			var handled bool
			var content io.Reader
			handled, content, writeErr = metadata.sidecar(ctx, entry.NameInArchive, data, entry.Size())
			if writeErr == nil && !handled {
				writeErr = x.write(entry.NameInArchive, entry.Mode(), entry.Size(), entry.LinkTarget, content)
			}
		} else {
			writeErr = x.write(entry.NameInArchive, entry.Mode(), entry.Size(), entry.LinkTarget, data)
		}
		closeErr := data.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	if metadata != nil {
		for key, attrs := range metadata.attributes {
			if key == "." {
				continue
			}
			actual := x.spelling[strings.ToLower(key)]
			if norm.NFD.String(actual) != key {
				return fmt.Errorf("metadata owner %s has no exact archive entry", attrs.owner)
			}
			attrs.owner = actual
		}
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

// codeSignature prefixes the extended attributes where codesign keeps the
// signature of code that is not a Mach-O.
const codeSignature = "com.apple.cs."

const maxSidecar = 64 << 20

// sidecarSignature refuses an AppleDouble sidecar that holds a code signature,
// which extraction would leave behind with the rest of the sidecar.
func sidecarSignature(entry archives.FileInfo) error {
	owner, sidecar := appledouble.OwnerName(entry.NameInArchive)
	if !sidecar || !entry.Mode().IsRegular() {
		return nil
	}
	if entry.Size() > maxSidecar {
		return fmt.Errorf("AppleDouble sidecar %s exceeds %d bytes", entry.NameInArchive, maxSidecar)
	}
	f, err := entry.Open()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSidecar+1))
	if err != nil {
		return err
	}
	if len(data) > maxSidecar {
		return fmt.Errorf("AppleDouble sidecar %s exceeds %d bytes", entry.NameInArchive, maxSidecar)
	}
	if !appledouble.Sniff(data) {
		return nil
	}
	attributes, err := appledouble.Decode(data)
	if err != nil {
		return fmt.Errorf("AppleDouble sidecar %s: %w", entry.NameInArchive, err)
	}
	for name := range attributes.Xattrs() {
		if strings.HasPrefix(name, codeSignature) {
			return fmt.Errorf("%s keeps its code signature in extended attributes, which extraction would discard", strings.TrimPrefix(owner, "__MACOSX/"))
		}
	}
	return nil
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
	folded := strings.ToLower(norm.NFD.String(name))
	for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
		key := strings.ToLower(norm.NFD.String(prefix))
		if prior, exists := x.spelling[key]; exists && norm.NFD.String(prior) != norm.NFD.String(prefix) {
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
