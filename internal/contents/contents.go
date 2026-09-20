// Package contents opens leased inputs without changing their source identity.
package contents

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/plugin"
)

// Source owns open filesystems and expanded archives for one preparation.
// Container contents are opened only when a consumer selects a path within them.
type Source struct {
	input     plugin.Artifact
	workspace string
	original  *os.Root
	tree      *os.Root
	image     *diskimage.Image
	expanded  string
}

// Node is a selected file or tree. FS does not outlive its Source.
// Local is empty for nodes read directly from a disk image.
type Node struct {
	FS    fs.ReadLinkFS
	Path  string
	Local string
	image *diskimage.Image
}

func Open(input plugin.Artifact, workspace string) (*Source, error) {
	if !filepath.IsAbs(input.Path) || !filepath.IsAbs(workspace) {
		return nil, errors.New("contents require absolute input and workspace paths")
	}
	info, err := os.Lstat(input.Path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() != input.Tree || !info.IsDir() && !info.Mode().IsRegular() {
		return nil, errors.New("leased input file type changed or is unsupported")
	}
	root, err := os.OpenRoot(filepath.Dir(input.Path))
	if err != nil {
		return nil, err
	}
	return &Source{input: input, workspace: workspace, original: root}, nil
}

func (s *Source) Close() error {
	var err error
	if s.tree != nil {
		err = s.tree.Close()
	}
	if s.image != nil {
		err = errors.Join(err, s.image.Close())
	}
	err = errors.Join(err, s.original.Close())
	if s.expanded != "" {
		err = errors.Join(err, os.RemoveAll(s.expanded))
	}
	return err
}

func (s *Source) whole() Node {
	return Node{FS: s.original.FS().(fs.ReadLinkFS), Path: filepath.Base(s.input.Path), Local: s.input.Path}
}

// At selects an exact path in the logical contents. An omitted path retains the
// original artifact; "." selects the contents root of a tree, archive or DMG.
// Scalar files, including PKGs, allow only themselves to be selected.
func (s *Source) At(ctx context.Context, name string) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	if name == "" {
		return s.whole(), nil
	}
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00\r\n\t") {
		return Node{}, fmt.Errorf("invalid content path %q", name)
	}
	tree, local, err := s.contents(ctx)
	if err != nil {
		return Node{}, err
	}
	if tree == nil {
		if name != "." {
			return Node{}, errors.New("cannot traverse a scalar input (PKGs remain package artifacts)")
		}
		return s.whole(), nil
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

// Select finds one application or installer using the existing native selection
// rules. Unlike At, an empty selection requests unambiguous payload discovery.
func (s *Source) Select(ctx context.Context, selection string) (Node, error) {
	if s.input.Tree && strings.EqualFold(filepath.Ext(s.input.Path), ".app") && (selection == "" || selection == ".") {
		return s.whole(), nil
	}
	tree, local, err := s.contents(ctx)
	if err != nil {
		return Node{}, err
	}
	if tree == nil {
		return s.whole(), nil
	}
	var name string
	if s.image != nil {
		name, err = s.image.Select(ctx, selection)
	} else {
		var selected string
		selected, err = archive.Select(local, selection)
		if err == nil {
			name, err = filepath.Rel(local, selected)
			name = filepath.ToSlash(name)
		}
	}
	if err != nil {
		return Node{}, err
	}
	return s.At(ctx, name)
}

func (s *Source) IsImage() bool     { return s.image != nil }
func (s *Source) Traversable() bool { return s.input.Tree || s.isImage() || isArchive(s.input) }

func (s *Source) isImage() bool {
	name := s.input.Filename
	if name == "" {
		name = filepath.Base(s.input.Path)
	}
	return !s.input.Tree && (s.input.Format == "dmg" || strings.EqualFold(filepath.Ext(name), ".dmg"))
}

func (s *Source) contents(ctx context.Context) (fs.ReadLinkFS, string, error) {
	if s.tree != nil {
		return s.tree.FS().(fs.ReadLinkFS), s.tree.Name(), nil
	}
	if s.image != nil {
		return s.image, "", nil
	}
	if s.isImage() {
		var err error
		s.image, err = diskimage.Open(ctx, s.input.Path)
		if err != nil {
			return nil, "", err
		}
		return s.image, "", nil
	}
	local := s.input.Path
	if !s.input.Tree {
		if !isArchive(s.input) {
			return nil, "", nil
		}
		work, err := os.MkdirTemp(s.workspace, ".contents-*")
		if err != nil {
			return nil, "", err
		}
		s.expanded = work
		local = filepath.Join(work, "tree")
		done := plugin.Stage(ctx, "Extracting archive")
		err = archive.Extract(ctx, s.input.Path, local)
		done(err)
		if err != nil {
			return nil, "", err
		}
	}
	var err error
	s.tree, err = os.OpenRoot(local)
	if err != nil {
		return nil, "", err
	}
	return s.tree.FS().(fs.ReadLinkFS), local, nil
}

func isArchive(input plugin.Artifact) bool {
	name := strings.ToLower(input.Filename)
	if name == "" {
		name = strings.ToLower(filepath.Base(input.Path))
	}
	return input.Format == "zip" || input.Format == "tar" || strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
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

// Materialize copies a DMG selection only when a native file is required.
func (n Node) Materialize(ctx context.Context, destination string) (string, error) {
	if n.Local != "" {
		return n.Local, nil
	}
	return n.image.Extract(ctx, destination, n.Path, nil)
}
