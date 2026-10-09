// Package contents opens leased inputs without changing their source identity.
package contents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/treefs"
	"github.com/woodleighschool/stemma/plugin"
)

// Source owns open filesystems and expanded archives for one preparation.
// Vendor archives and disk images are opened when a consumer selects their contents.
type Source struct {
	input      plugin.Artifact
	workspace  string
	original   *os.Root
	container  string
	tree       *os.Root
	image      *diskimage.Image
	archive    *archive.Tree
	packed     fs.ReadLinkFS
	packedFile *treefs.File
}

// Node is a selected file or tree. FS does not outlive its Source.
// Local is empty for nodes read directly from a container.
type Node struct {
	FS    fs.ReadLinkFS
	Path  string
	Local string
	image *diskimage.Image
}

func Open(ctx context.Context, input plugin.Artifact, workspace string) (*Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(input.Path) || !filepath.IsAbs(workspace) {
		return nil, errors.New("contents require absolute input and workspace paths")
	}
	if input.ContentRoot != "" && (!fs.ValidPath(input.ContentRoot) || strings.ContainsAny(input.ContentRoot, "\\\x00\r\n\t")) {
		return nil, errors.New("invalid resolved content root")
	}
	info, err := os.Lstat(input.Path)
	if err != nil {
		return nil, err
	}
	if input.Encoding != "" && (input.Encoding != "tar" || !input.Tree) {
		return nil, errors.New("unsupported leased input encoding")
	}
	if info.IsDir() != (input.Tree && input.Encoding == "") || !info.IsDir() && !info.Mode().IsRegular() {
		return nil, errors.New("leased input file type changed or is unsupported")
	}
	root, err := os.OpenRoot(filepath.Dir(input.Path))
	if err != nil {
		return nil, err
	}
	source := &Source{input: input, workspace: workspace, original: root}
	if input.Encoding == "tar" {
		file, err := treefs.OpenFile(treefs.Local(root), filepath.Base(input.Path))
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		source.packedFile = file
		source.packed, err = archive.IndexTar(ctx, file, info.Size(), input.Filename, fs.FileMode(input.Mode))
		if err != nil {
			return nil, errors.Join(err, source.Close())
		}
	}
	if !input.Tree {
		file, err := root.Open(filepath.Base(input.Path))
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		image := diskimage.HasTrailer(file, info.Size())
		_ = file.Close()
		if image || input.Format == "dmg" || strings.EqualFold(filepath.Ext(input.Filename), ".dmg") {
			source.container = "dmg"
		} else if archived, err := archive.IsArchive(ctx, input.Path); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("identify input: %w", err)
		} else if archived {
			source.container = "archive"
		}
	}
	return source, nil
}

func (s *Source) Close() error {
	var err error
	if s.packedFile != nil {
		err = s.packedFile.Close()
	}
	if s.tree != nil {
		err = errors.Join(err, s.tree.Close())
	}
	if s.image != nil {
		err = errors.Join(err, s.image.Close())
	}
	err = errors.Join(err, s.original.Close())
	if s.archive != nil {
		err = errors.Join(err, s.archive.Close())
	}
	return err
}

func (s *Source) whole() Node {
	if s.packed != nil {
		return Node{FS: s.packed, Path: s.input.Filename}
	}
	return Node{FS: treefs.Local(s.original), Path: filepath.Base(s.input.Path), Local: s.input.Path}
}

// At selects an exact path in the logical contents. A resolver-selected root
// scopes every selection. Otherwise an omitted path retains the original
// artifact and "." selects the contents root of a tree, archive or DMG.
// Scalar files, including PKGs, allow only themselves to be selected.
func (s *Source) At(ctx context.Context, name string) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	if name == "" && s.input.ContentRoot == "" {
		return s.whole(), nil
	}
	if name == "" {
		name = "."
	}
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00\r\n\t") {
		return Node{}, fmt.Errorf("invalid content path %q", name)
	}
	tree, local, err := s.contents(ctx)
	if err != nil {
		return Node{}, err
	}
	if tree == nil {
		if s.input.ContentRoot != "" {
			return Node{}, errors.New("resolved content root requires a tree, archive or disk image")
		}
		if name != "." {
			return Node{}, errors.New("cannot traverse a scalar input (PKGs remain package artifacts)")
		}
		return s.whole(), nil
	}
	if s.input.ContentRoot != "" {
		root := Node{FS: tree, Path: s.input.ContentRoot}
		info, err := root.Stat()
		if err != nil {
			return Node{}, fmt.Errorf("resolved content root: %w", err)
		}
		if !info.IsDir() {
			return Node{}, errors.New("resolved content root must be a directory, not a symlink or file")
		}
		name = path.Join(s.input.ContentRoot, name)
	}
	node := Node{FS: tree, Path: name, image: s.image}
	if local != "" {
		node.Local = filepath.Join(local, filepath.FromSlash(name))
	}
	if _, err := node.Stat(); err != nil {
		return Node{}, err
	}
	return node, nil
}

// Artifact returns the leased input.
func (s *Source) Artifact() plugin.Artifact { return s.input }

func (s *Source) IsImage() bool     { return s.image != nil }
func (s *Source) Traversable() bool { return s.input.Tree || s.container != "" }

func (s *Source) contents(ctx context.Context) (fs.ReadLinkFS, string, error) {
	if s.packed != nil {
		sub, err := fs.Sub(s.packed, s.input.Filename)
		if err != nil {
			return nil, "", err
		}
		return sub.(fs.ReadLinkFS), "", nil
	}
	if s.archive != nil {
		return s.archive, s.archive.Name(), nil
	}
	if s.tree != nil {
		return treefs.Local(s.tree), s.tree.Name(), nil
	}
	if s.image != nil {
		return s.image, "", nil
	}
	if s.container == "dmg" {
		var err error
		s.image, err = diskimage.Open(ctx, s.input.Path)
		if err != nil {
			return nil, "", err
		}
		return s.image, "", nil
	}
	local := s.input.Path
	if !s.input.Tree {
		if s.container != "archive" {
			return nil, "", nil
		}
		done := plugin.Stage(ctx, "Extracting archive", plugin.Detail(filepath.Base(s.input.Path)))
		var err error
		s.archive, err = archive.Open(ctx, s.input.Path, s.workspace)
		done(err)
		if err != nil {
			return nil, "", err
		}
		return s.archive, s.archive.Name(), nil
	}
	var err error
	s.tree, err = os.OpenRoot(local)
	if err != nil {
		return nil, "", err
	}
	return treefs.Local(s.tree), local, nil
}

// Stat rejects traversal through links. A selected link itself is returned so
// composition can preserve it without dereferencing it.
func (n Node) Stat() (fs.FileInfo, error) {
	for parent := path.Dir(n.Path); parent != "."; parent = path.Dir(parent) {
		info, err := n.FS.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, errors.New("content path traverses a symlink or nondirectory")
		}
	}
	return n.FS.Lstat(n.Path)
}

// Materialize copies a selection only when a native file is required.
func (n Node) Materialize(ctx context.Context, destination string) (name string, err error) {
	if n.Local != "" {
		return n.Local, nil
	}
	if n.image != nil {
		return n.image.Extract(ctx, destination, n.Path, nil)
	}
	f, err := treefs.OpenFile(n.FS, n.Path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", err
	}
	leaf, err := filepath.Localize(path.Base(n.Path))
	if err != nil {
		return "", fmt.Errorf("selection filename: %w", err)
	}
	name = filepath.Join(destination, leaf)
	output, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return "", err
	}
	_, err = io.Copy(output, fileio.Reader{Context: ctx, Reader: f})
	err = errors.Join(err, output.Close())
	if err != nil {
		_ = os.Remove(name)
	}
	return name, err
}
