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
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/plugin"
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
		return nil
	}
	if len(stage.metadata) >= pkgbuild.MaxEntries {
		return errors.New("package exceeds payload entry limit")
	}
	if err := os.Mkdir(filepath.Join(stage.root, "Payload", filepath.FromSlash(name)), 0o755); err != nil {
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
	file, err := os.OpenFile(filepath.Join(stage.root, "Payload", filepath.FromSlash(name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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

func (stage *layout) copy(ctx context.Context, root *os.Root, source, destination string, attrs pkgbuild.EntryMetadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, info, err := checkedInput(root, source)
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
	children, err := file.ReadDir(pkgbuild.MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(children) > pkgbuild.MaxEntries-len(stage.metadata) {
		return errors.New("package exceeds payload entry limit")
	}
	slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	attrs.Mode = nil
	for _, child := range children {
		if err := stage.copy(ctx, root, path.Join(source, child.Name()), path.Join(destination, child.Name()), attrs); err != nil {
			return err
		}
	}
	return nil
}

func (stage *layout) script(ctx context.Context, destination string, ref InputFileRef, inputs map[string]plugin.Artifact) error {
	root, selected, err := openInput(ref, inputs)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	file, info, err := checkedInput(root, selected)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if !info.Mode().IsRegular() || info.Size() > pkgbuild.MaxScriptSize {
		return errors.New("script must be a regular file within the script size limit")
	}
	if err := os.MkdirAll(filepath.Join(stage.root, "Scripts"), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(stage.root, filepath.FromSlash(destination)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, io.LimitReader(fileio.Reader{Context: ctx, Reader: file}, info.Size()+1))
	closeErr := out.Close()
	if n != info.Size() {
		return errors.New("script changed size while reading")
	}
	return errors.Join(copyErr, closeErr)
}

func openInput(ref InputFileRef, inputs map[string]plugin.Artifact) (*os.Root, string, error) {
	artifact, exists := inputs[ref.Input]
	if !exists || !filepath.IsAbs(artifact.Path) {
		return nil, "", fmt.Errorf("input %q requires an absolute leased path", ref.Input)
	}
	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return nil, "", err
	}
	if info.IsDir() != artifact.Tree || !info.IsDir() && !info.Mode().IsRegular() {
		return nil, "", errors.New("leased input file type changed or is unsupported")
	}
	if !artifact.Tree {
		if ref.Path != "" && ref.Path != "." {
			return nil, "", errors.New("path selection requires a tree input")
		}
		root, err := os.OpenRoot(filepath.Dir(artifact.Path))
		return root, filepath.Base(artifact.Path), err
	}
	selected := ref.Path
	if selected == "" {
		selected = "."
	}
	root, err := os.OpenRoot(artifact.Path)
	return root, selected, err
}

func checkedInput(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := root.Lstat(parent)
		if err != nil {
			return nil, nil, err
		}
		if !info.IsDir() {
			return nil, nil, errors.New("input path traverses a symlink or nondirectory")
		}
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, nil, errors.New("input contains an unsupported file type")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	actual, err := file.Stat()
	if err == nil && !os.SameFile(info, actual) {
		err = errors.New("input changed while opening")
	}
	if err == nil {
		err = archive.CheckMetadata(file, actual)
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, actual, nil
}
