package pkgbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/fileio"
)

// Layout stages one payload or scripts tree for Build. Staged entries keep
// their bytes on disk and their ownership and permissions in Metadata, so a
// tree stages alike on every host.
type Layout struct {
	root     string
	metadata map[string]EntryMetadata
	claimed  map[string]bool
	bytes    int64
}

// NewLayout creates root, which must not exist, as the staged tree.
func NewLayout(root string) (*Layout, error) {
	stage := &Layout{root: root, metadata: map[string]EntryMetadata{}, claimed: map[string]bool{}}
	return stage, stage.parents(".")
}

// Metadata returns the archive metadata of every staged entry, keyed as
// Options.Metadata is.
func (stage *Layout) Metadata() map[string]EntryMetadata { return stage.metadata }

// Directory declares a directory, mode 0755 unless attrs says otherwise.
func (stage *Layout) Directory(name string, attrs EntryMetadata) error {
	if stage.claimed[name] {
		return fmt.Errorf("overlapping payload destination %q", name)
	}
	if err := stage.parents(name); err != nil {
		return err
	}
	if attrs.Mode == nil {
		mode := uint32(0o755)
		attrs.Mode = &mode
	}
	stage.metadata[name], stage.claimed[name] = attrs, true
	return nil
}

func (stage *Layout) parents(name string) error {
	if name != "." {
		if err := stage.parents(path.Dir(name)); err != nil {
			return err
		}
	}
	if _, exists := stage.metadata[name]; exists {
		info, err := os.Lstat(filepath.Join(stage.root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("destination parent %q is a symlink or nondirectory", name)
		}
		return nil
	}
	if len(stage.metadata) >= MaxEntries {
		return errors.New("package exceeds payload entry limit")
	}
	if err := os.Mkdir(filepath.Join(stage.root, filepath.FromSlash(name)), 0o755); err != nil {
		return err
	}
	mode := uint32(0o755)
	stage.metadata[name] = EntryMetadata{Mode: &mode}
	return nil
}

// File stages size bytes of source as a regular file. Its permissions are
// those of attrs, or of mode.
func (stage *Layout) File(ctx context.Context, name string, source io.Reader, size int64, attrs EntryMetadata, mode os.FileMode) error {
	if stage.claimed[name] {
		return fmt.Errorf("overlapping payload destination %q", name)
	}
	if size < 0 || size > MaxPayloadSize-stage.bytes {
		return errors.New("package exceeds payload size limit")
	}
	if _, exists := stage.metadata[name]; exists {
		return fmt.Errorf("payload file %q overlaps a directory", name)
	}
	if err := stage.parents(path.Dir(name)); err != nil {
		return err
	}
	if len(stage.metadata) >= MaxEntries {
		return errors.New("package exceeds payload entry limit")
	}
	file, err := os.OpenFile(filepath.Join(stage.root, filepath.FromSlash(name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(file, io.LimitReader(fileio.Reader{Context: ctx, Reader: source}, size+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if n != size {
		return errors.New("input changed size while reading")
	}
	if attrs.Mode == nil {
		permissions := uint32(mode.Perm())
		attrs.Mode = &permissions
	}
	stage.bytes += size
	stage.metadata[name], stage.claimed[name] = attrs, true
	return nil
}

// checkSignatureAttributes rejects signing metadata the payload would discard.
func checkSignatureAttributes(ctx context.Context, node contents.Node, file fs.File) error {
	attributes, ok := node.FS.(interface {
		XattrValues(name string) (map[string]appledouble.Value, error)
	})
	if !ok {
		if local, ok := file.(*os.File); ok {
			return archive.CheckXattrs(ctx, local)
		}
		return nil
	}
	xattrs, err := attributes.XattrValues(node.Path)
	if err != nil {
		return err
	}
	for name := range xattrs {
		if strings.HasPrefix(name, "com.apple.cs.") {
			return fmt.Errorf("%s keeps its code signature in extended attributes, which a package payload cannot carry", node.Path)
		}
	}
	return nil
}

// Copy stages a file or tree at destination, keeping its permissions and the
// relative symlinks that stay inside it. attrs owns the subtree and its mode
// applies to the root.
func (stage *Layout) Copy(ctx context.Context, node contents.Node, destination string, attrs EntryMetadata) error {
	info, err := node.Stat()
	if err != nil {
		return err
	}
	boundary := node.Path
	if !info.IsDir() {
		boundary = path.Dir(boundary)
	}
	return stage.copyNode(ctx, node, destination, attrs, boundary)
}

func (stage *Layout) copyNode(ctx context.Context, node contents.Node, destination string, attrs EntryMetadata, boundary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := node.Stat()
	if err != nil {
		return err
	}
	if err := archive.CheckMode(info); err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		link, err := node.FS.ReadLink(node.Path)
		if err != nil {
			return err
		}
		resolved := path.Join(path.Dir(node.Path), link)
		if boundary != "." && resolved != boundary && !strings.HasPrefix(resolved, boundary+"/") {
			return errors.New("symlink escapes selected content")
		}
		if path.IsAbs(link) || strings.ContainsAny(link, "\\\x00") || !validPath(path.Join(path.Dir(node.Path), link)) || !validPath(path.Join(path.Dir(destination), link)) {
			return errors.New("escaping content symlink")
		}
		if attrs.Mode == nil {
			mode := uint32(info.Mode().Perm())
			attrs.Mode = &mode
		}
		return stage.Symlink(destination, link, attrs)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("input contains an unsupported file type")
	}
	file, err := node.FS.Open(node.Path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if !info.IsDir() {
		if err := checkSignatureAttributes(ctx, node, file); err != nil {
			return err
		}
		return stage.File(ctx, destination, file, info.Size(), attrs, info.Mode())
	}
	if attrs.Mode == nil {
		mode := uint32(info.Mode().Perm())
		attrs.Mode = &mode
	}
	if err := stage.Directory(destination, attrs); err != nil {
		return err
	}
	dir, ok := file.(fs.ReadDirFile)
	if !ok {
		return errors.New("input directory cannot be read")
	}
	children, err := dir.ReadDir(MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(children) > MaxEntries-len(stage.metadata) {
		return errors.New("package exceeds entry limit")
	}
	slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	attrs.Mode = nil
	for _, child := range children {
		childNode := contents.Node{FS: node.FS, Path: path.Join(node.Path, child.Name())}
		if err := stage.copyNode(ctx, childNode, path.Join(destination, child.Name()), attrs, boundary); err != nil {
			return err
		}
	}
	return nil
}

// ValidSymlink reports whether a link at name can point at target: a relative
// path that stays inside the tree.
func ValidSymlink(name, target string) bool {
	return !path.IsAbs(target) && len(target) <= 4096 && !strings.ContainsAny(target, "\\\x00\r\n\t") && validPath(path.Join(path.Dir(name), target))
}

// Symlink stages a relative link.
func (stage *Layout) Symlink(name, target string, attrs EntryMetadata) error {
	if !ValidSymlink(name, target) {
		return errors.New("symlink must be relative and confined to the package root")
	}
	if stage.claimed[name] {
		return fmt.Errorf("overlapping payload destination %q", name)
	}
	if _, exists := stage.metadata[name]; exists {
		return fmt.Errorf("symlink %q overlaps a directory", name)
	}
	if err := stage.parents(path.Dir(name)); err != nil {
		return err
	}
	if len(stage.metadata) >= MaxEntries {
		return errors.New("package exceeds entry limit")
	}
	if err := os.Symlink(filepath.FromSlash(target), filepath.Join(stage.root, filepath.FromSlash(name))); err != nil {
		return err
	}
	if attrs.Mode == nil {
		mode := uint32(0o777)
		attrs.Mode = &mode
	}
	stage.metadata[name], stage.claimed[name] = attrs, true
	return nil
}
