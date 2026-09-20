package pkgbuild

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/woodleighschool/stemma/internal/archive"
)

// Package links follow POSIX semantics. Windows cleans parent segments before
// following links, so asking the host to resolve a chain can hide an escape.
func validateLink(root *os.Root, name string) error {
	pending := strings.Split(name, "/")
	var resolved []string
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return errors.New("symlink escapes archive")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		resolved = append(resolved, part)
		name := strings.Join(resolved, "/")
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			links++
			if links > 40 {
				return errors.New("symlink chain exceeds 40 links")
			}
			target, err := archive.Readlink(root, name)
			if err != nil {
				return err
			}
			resolved = resolved[:len(resolved)-1]
			pending = append(strings.Split(target, "/"), pending...)
		}
	}
	return nil
}
