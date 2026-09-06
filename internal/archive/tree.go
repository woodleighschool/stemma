package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/fileio"
)

// Pack writes a canonical TAR tree, retaining modes and confined symlinks.
// Unsupported metadata is rejected rather than silently discarded.
func Pack(ctx context.Context, root string, output io.Writer) error {
	scoped, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	return PackSelected(ctx, scoped, nil, output)
}

// PackSelected writes selected entries and their parent directories in canonical
// order. Nil names selects the whole scoped root; directory names alone do not
// select their contents. Symlink parents are rejected rather than dereferenced.
func PackSelected(ctx context.Context, root *os.Root, names []string, output io.Writer) error {
	if names == nil {
		directory, err := root.Open(".")
		if err != nil {
			return err
		}
		info, statErr := directory.Stat()
		if statErr == nil {
			statErr = CheckMetadata(directory, info)
		}
		closeErr := directory.Close()
		if statErr != nil {
			return statErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	var selected map[string]bool
	if names != nil {
		selected = map[string]bool{}
		for _, name := range names {
			if _, err := safeName(name); err != nil {
				return err
			}
			selected[name] = true
			for parent := filepath.ToSlash(filepath.Dir(name)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
				info, err := root.Lstat(parent)
				if err != nil {
					return err
				}
				if !info.IsDir() {
					return fmt.Errorf("selected path %s traverses a symlink or nondirectory parent", name)
				}
				selected[parent] = true
			}
			if len(selected) > maxEntries {
				return fmt.Errorf("tree exceeds %d entries", maxEntries)
			}
		}
	}
	w := tar.NewWriter(output)
	var total int64
	count := 0
	visited := map[string]bool{".": true}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if selected != nil && !selected[name] {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		visited[name] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maxEntries {
			return fmt.Errorf("tree exceeds %d entries", maxEntries)
		}
		if _, err := safeName(name); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if err := checkSymlinkMetadata(root, name, info); err != nil {
				return fmt.Errorf("tree entry %s: %w", name, err)
			}
			target, err = root.Readlink(name)
			if err != nil {
				return err
			}
			target = filepath.ToSlash(target)
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
			if filepath.IsAbs(target) || !filepath.IsLocal(resolved) || strings.Contains(target, "\\") {
				return fmt.Errorf("escaping symlink %s", name)
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported tree entry %s", name)
		}
		var f *os.File
		if info.Mode()&os.ModeSymlink == 0 {
			f, err = root.Open(name)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			actual, err := f.Stat()
			if err != nil {
				return err
			}
			if !os.SameFile(info, actual) {
				return fmt.Errorf("tree changed while opening %s", name)
			}
			if err := CheckMetadata(f, actual); err != nil {
				return fmt.Errorf("tree entry %s: %w", name, err)
			}
			info = actual
		}
		h, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return err
		}
		h.Name = name
		h.ModTime = time.Unix(0, 0)
		h.AccessTime = time.Time{}
		h.ChangeTime = time.Time{}
		h.Uid = 0
		h.Gid = 0
		h.Uname = ""
		h.Gname = ""
		h.Format = tar.FormatPAX
		if err := w.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if info.Size() > maxBytes-total {
				return fmt.Errorf("tree exceeds size limit")
			}
			total += info.Size()
			n, err := io.Copy(w, io.LimitReader(fileio.Reader{Context: ctx, Reader: f}, info.Size()+1))
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if n != info.Size() {
				return fmt.Errorf("tree changed during import: %s", name)
			}
		}
		return nil
	})
	closeErr := w.Close()
	if err != nil {
		return err
	}
	for name := range selected {
		if !visited[name] {
			return fmt.Errorf("selected input changed during import: %s", name)
		}
	}
	return closeErr
}
