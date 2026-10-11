package inspect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/treefs"
)

// Member writes the file at name inside a leased input to w. A directory
// writes the names it holds, one a line, and "." names the input's root.
//
// A flat package reads as a directory wherever it sits: of its archive
// entries, and of the files in each component's Scripts and Payload archives.
// The input is read where it is, except that an archive is expanded first.
func Member(ctx context.Context, source *contents.Source, name string, w io.Writer) error {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00\r\n\t") {
		return fmt.Errorf("invalid member path %q", name)
	}
	if !source.Traversable() {
		whole, err := source.At(ctx, "")
		if err != nil {
			return err
		}
		return readFile(ctx, whole.FS, whole.Path, source.Artifact().Filename, name, w)
	}
	root, err := source.At(ctx, ".")
	if err != nil {
		return err
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		reached := path.Join(parts[:i+1]...)
		full := path.Join(root.Path, reached)
		info, err := root.FS.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			held, listErr := names(root.FS, path.Dir(full))
			if listErr != nil {
				return err
			}
			holder := path.Dir(reached)
			if holder == "." {
				holder = "the input"
			}
			return apple.NoMember(holder, parts[i], held)
		}
		if err != nil {
			return err
		}
		last := i == len(parts)-1
		switch {
		case info.IsDir() && last:
			held, err := names(root.FS, full)
			if err != nil {
				return err
			}
			return apple.WriteNames(w, held)
		case info.IsDir():
		case info.Mode().IsRegular():
			var member string
			if !last {
				member = path.Join(parts[i+1:]...)
			}
			return readFile(ctx, root.FS, full, reached, member, w)
		default:
			return fmt.Errorf("%s is not a regular file or directory", reached)
		}
	}
	return nil
}

func readFile(ctx context.Context, fsys fs.ReadLinkFS, file, label, member string, w io.Writer) (err error) {
	f, err := treefs.OpenFile(fsys, file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err == nil && string(magic[:]) == "xar!" {
		if member == "" {
			member = "."
		}
		if err := apple.ReadPackageMember(ctx, f, info.Size(), member, w); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}
	if member != "" {
		return fmt.Errorf("%s is not a package, disk image, archive or directory, so nothing is read inside it", label)
	}
	_, err = io.Copy(w, fileio.Reader{Context: ctx, Reader: f})
	return err
}

// names lists a directory, with a slash after the directories it holds.
func names(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	held := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		held = append(held, name)
	}
	return held, nil
}
