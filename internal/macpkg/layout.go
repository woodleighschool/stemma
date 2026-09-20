package macpkg

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

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
)

type layout struct {
	root     string
	metadata map[string]pkgbuild.EntryMetadata
	claimed  map[string]bool
	bytes    int64
}

func (stage *layout) directory(name string, attrs pkgbuild.EntryMetadata) error {
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

func (stage *layout) parents(name string) error {
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
	if len(stage.metadata) >= pkgbuild.MaxEntries {
		return errors.New("package exceeds payload entry limit")
	}
	if err := os.Mkdir(filepath.Join(stage.root, filepath.FromSlash(name)), 0o755); err != nil {
		return err
	}
	mode := uint32(0o755)
	stage.metadata[name] = pkgbuild.EntryMetadata{Mode: &mode}
	return nil
}

func (stage *layout) content(ctx context.Context, name string, source io.Reader, size int64, attrs pkgbuild.EntryMetadata, mode os.FileMode) error {
	if stage.claimed[name] {
		return fmt.Errorf("overlapping payload destination %q", name)
	}
	if size < 0 || size > pkgbuild.MaxPayloadSize-stage.bytes {
		return errors.New("package exceeds payload size limit")
	}
	if _, exists := stage.metadata[name]; exists {
		return fmt.Errorf("payload file %q overlaps a directory", name)
	}
	if err := stage.parents(path.Dir(name)); err != nil {
		return err
	}
	if len(stage.metadata) >= pkgbuild.MaxEntries {
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

func (stage *layout) copy(ctx context.Context, node contents.Node, destination string, attrs pkgbuild.EntryMetadata) error {
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

func (stage *layout) copyNode(ctx context.Context, node contents.Node, destination string, attrs pkgbuild.EntryMetadata, boundary string) error {
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
		if stage.claimed[destination] {
			return fmt.Errorf("overlapping destination %q", destination)
		}
		if _, exists := stage.metadata[destination]; exists {
			return fmt.Errorf("symlink %q overlaps a directory", destination)
		}
		if err := stage.parents(path.Dir(destination)); err != nil {
			return err
		}
		if len(stage.metadata) >= pkgbuild.MaxEntries {
			return errors.New("package exceeds entry limit")
		}
		if err := os.Symlink(filepath.FromSlash(link), filepath.Join(stage.root, filepath.FromSlash(destination))); err != nil {
			return err
		}
		if attrs.Mode == nil {
			mode := uint32(info.Mode().Perm())
			attrs.Mode = &mode
		}
		stage.metadata[destination], stage.claimed[destination] = attrs, true
		return nil
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
		return stage.content(ctx, destination, file, info.Size(), attrs, info.Mode())
	}
	if attrs.Mode == nil {
		mode := uint32(info.Mode().Perm())
		attrs.Mode = &mode
	}
	if err := stage.directory(destination, attrs); err != nil {
		return err
	}
	dir, ok := file.(fs.ReadDirFile)
	if !ok {
		return errors.New("input directory cannot be read")
	}
	children, err := dir.ReadDir(pkgbuild.MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(children) > pkgbuild.MaxEntries-len(stage.metadata) {
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
