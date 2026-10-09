// Package treefs reads confined trees through directory-scoped handles.
package treefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Local borrows a host root. The caller retains ownership of root.
func Local(root *os.Root) fs.ReadLinkFS { return local{root.FS().(fs.ReadLinkFS), root} }

type local struct {
	fs.ReadLinkFS

	root *os.Root
}

func (l local) ReadLink(name string) (string, error) {
	target, err := l.ReadLinkFS.ReadLink(name)
	return filepath.ToSlash(target), err
}

func (l local) Sub(name string) (fs.FS, error) {
	root, err := l.root.OpenRoot(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	return &Directory{ReadLinkFS: Local(root), close: root.Close}, nil
}

// Directory owns a view of a real directory. Close rechecks the directory
// entry and releases its handles.
type Directory struct {
	fs.ReadLinkFS

	close func() error
}

func (d *Directory) Sub(name string) (fs.FS, error) { return fs.Sub(d.ReadLinkFS, name) }
func (d *Directory) Close() error {
	if d.close == nil {
		return nil
	}
	release := d.close
	d.close = nil
	return release()
}

// OpenDir rejects symlink components and owns only the directory it opens.
// Virtual filesystems retain their own Sub implementation and need no handles.
func OpenDir(fsys fs.ReadLinkFS, name string) (*Directory, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "opendir", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		before, err := fsys.Lstat(".")
		if err != nil {
			return nil, err
		}
		return &Directory{ReadLinkFS: fsys, close: func() error {
			after, err := fsys.Lstat(".")
			if err == nil && !same(before, after) {
				err = errors.New("directory changed during read")
			}
			return err
		}}, nil
	}
	parent, leaf := path.Split(name)
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		parent = "."
	}
	dir, err := OpenDir(fsys, parent)
	if err != nil {
		return nil, err
	}
	before, err := dir.Lstat(leaf)
	if err == nil && !before.IsDir() {
		err = fmt.Errorf("%s is a symlink or nondirectory", name)
	}
	if err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	sub, err := fs.Sub(dir, leaf)
	if err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	links, ok := sub.(fs.ReadLinkFS)
	closeSub := func() error {
		if closer, ok := sub.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	}
	if !ok {
		return nil, errors.Join(fmt.Errorf("%s does not report symlinks", name), closeSub(), dir.Close())
	}
	check := func() error {
		entry, err := dir.Lstat(leaf)
		if err != nil {
			return err
		}
		held, err := links.Lstat(".")
		if err != nil {
			return err
		}
		if !same(before, entry) || !same(before, held) {
			return fmt.Errorf("directory changed during read: %s", name)
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, errors.Join(err, closeSub(), dir.Close())
	}
	return &Directory{ReadLinkFS: links, close: func() error { return errors.Join(check(), closeSub(), dir.Close()) }}, nil
}

func same(a, b fs.FileInfo) bool {
	return a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && (!os.SameFile(a, a) || os.SameFile(a, b))
}

// File holds the regular file selected by its directory entry. Close rejects
// replacement, size, mode or modification-time changes observed during reads.
type File struct {
	fs.File

	dir    *Directory
	name   string
	before fs.FileInfo
}

// OpenFile opens a regular file without accepting symlinks in its path.
func OpenFile(fsys fs.ReadLinkFS, name string) (*File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	dir := &Directory{ReadLinkFS: fsys}
	if parent := path.Dir(name); parent != "." {
		var err error
		dir, err = OpenDir(fsys, parent)
		if err != nil {
			return nil, err
		}
	}
	leaf := path.Base(name)
	before, err := dir.Lstat(leaf)
	if err == nil && !before.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	if err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	f, err := dir.Open(leaf)
	if err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	file := &File{File: f, dir: dir, name: leaf, before: before}
	if err := file.check(); err != nil {
		return nil, errors.Join(err, f.Close(), dir.Close())
	}
	return file, nil
}

func (f *File) ReadAt(p []byte, offset int64) (int, error) {
	r, ok := f.File.(io.ReaderAt)
	if !ok {
		return 0, errors.New("filesystem does not support reading at offsets")
	}
	return r.ReadAt(p, offset)
}

func (f *File) check() error {
	held, err := f.Stat()
	if err != nil {
		return err
	}
	entry, err := f.dir.Lstat(f.name)
	if err != nil {
		return err
	}
	if !same(f.before, held) || !same(f.before, entry) {
		return fmt.Errorf("file changed during read: %s", f.name)
	}
	return nil
}

func (f *File) Close() error { return errors.Join(f.check(), f.File.Close(), f.dir.Close()) }

// Walk visits entries in lexical order, with each entry's parent held open.
// The parent is borrowed for the callback. SkipDir skips a directory's contents.
func Walk(fsys fs.ReadLinkFS, visit func(string, fs.ReadLinkFS, fs.DirEntry) error) error {
	return walk(fsys, 1, visit)
}

// WalkConcurrent visits local nondirectory entries with at most eight workers.
// Directories remain open until their callbacks finish. Directory visits are
// serial, and virtual filesystems retain serial access.
func WalkConcurrent(fsys fs.ReadLinkFS, visit func(string, fs.ReadLinkFS, fs.DirEntry) error) error {
	workers := 1
	if native(fsys) {
		workers = 8
	}
	return walk(fsys, workers, visit)
}

func native(fsys fs.ReadLinkFS) bool {
	switch fsys := fsys.(type) {
	case local:
		return true
	case *Directory:
		return native(fsys.ReadLinkFS)
	default:
		return false
	}
}

func walk(fsys fs.ReadLinkFS, workers int, visit func(string, fs.ReadLinkFS, fs.DirEntry) error) error {
	var descend func(fs.ReadLinkFS, string) error
	descend = func(dir fs.ReadLinkFS, prefix string) (err error) {
		before, err := dir.Lstat(".")
		if err != nil {
			return err
		}
		var group errgroup.Group
		group.SetLimit(workers)
		defer func() {
			err = errors.Join(err, group.Wait())
			after, statErr := dir.Lstat(".")
			if statErr == nil && !same(before, after) {
				statErr = fmt.Errorf("directory changed during walk: %s", prefix)
			}
			err = errors.Join(err, statErr)
		}()
		entries, err := fs.ReadDir(dir, ".")
		if err != nil {
			return err
		}
		check := func(entry fs.DirEntry) error {
			before, err := entry.Info()
			if err != nil {
				return err
			}
			name := path.Join(prefix, entry.Name())
			err = visit(name, dir, fs.FileInfoToDirEntry(before))
			if err != nil && !errors.Is(err, fs.SkipDir) {
				return err
			}
			after, statErr := dir.Lstat(entry.Name())
			if statErr != nil {
				return statErr
			}
			if !same(before, after) {
				return fmt.Errorf("entry changed during walk: %s", name)
			}
			return err
		}
		for _, entry := range entries {
			if workers > 1 && !entry.IsDir() {
				group.Go(func() error { return check(entry) })
				continue
			}
			if err := group.Wait(); err != nil {
				return err
			}
			err := check(entry)
			if errors.Is(err, fs.SkipDir) && entry.IsDir() {
				continue
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				child, err := OpenDir(dir, entry.Name())
				if err != nil {
					return err
				}
				err = descend(child, path.Join(prefix, entry.Name()))
				if err = errors.Join(err, child.Close()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return descend(fsys, "")
}
