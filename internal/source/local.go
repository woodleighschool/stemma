package source

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

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

// absolutePath reports whether a declared path names a host location instead of
// one resolved from the project. A slash-rooted path counts on every platform so
// that a catalog reads the same way everywhere.
func absolutePath(name string) bool { return path.IsAbs(name) || filepath.IsAbs(name) }

// resolvePath resolves a declared path with ordinary filesystem semantics:
// absolute paths name a host location, and relative paths resolve from the
// resource file's directory. Relative results stay relative to the project, so
// they identify the same input on any checkout.
func resolvePath(base, name string) (string, error) {
	if strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("input path must not contain control characters")
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name), nil
	}
	if path.IsAbs(name) {
		return path.Clean(name), nil
	}
	if path.IsAbs(base) || strings.ContainsAny(base+name, "\\:") {
		return "", errors.New("input path must be absolute or a slash-separated relative path")
	}
	resolved := path.Join(base, name)
	if resolved == "" {
		resolved = "."
	}
	return resolved, nil
}

// projectPath resolves a tree root, which stays inside the project because its
// contents are selected by globs walked from that root without following links.
func projectPath(base, name string) (string, error) {
	resolved, err := resolvePath(base, name)
	if err != nil {
		return "", err
	}
	if absolutePath(resolved) || !safeRelative(resolved) {
		return "", errors.New("local input base must stay inside the project")
	}
	return resolved, nil
}

func safeRelative(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:\x00\r\n") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// readLocal hashes a file or local tree as it is now.
func (m *Manager) readLocal(ctx context.Context, s fileRequest) (content Content, err error) {
	content.Filename = s.Filename
	if s.Include != nil {
		done := plugin.Stage(ctx, "Reading local inputs", plugin.Detail(s.Path))
		defer func() { done(err) }()
		project, err := os.OpenRoot(m.Root)
		if err != nil {
			return content, err
		}
		defer func() { _ = project.Close() }()
		base := s.Path
		if base == "" {
			base = "."
		}
		info, err := project.Lstat(base)
		if err != nil {
			return content, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return content, errors.New("local input base must not be a symlink")
		}
		content.Tree, content.Mode = info.IsDir(), uint32(info.Mode().Perm())
		if content.Filename == "" {
			content.Filename = path.Base(s.Path)
			if content.Filename == "." || content.Filename == "" {
				content.Filename = "local"
			}
		}
		if !validFilename(content.Filename) {
			return content, errors.New("input has no safe filename; set filename explicitly")
		}
		root, err := project.OpenRoot(base)
		if err != nil {
			return content, err
		}
		defer func() { _ = root.Close() }()
		var names []string
		seen := map[string]bool{}
		for _, pattern := range s.Include {
			matched := false
			err := doublestar.GlobWalk(root.FS(), pattern, func(name string, _ fs.DirEntry) error {
				matched = true
				if !seen[name] {
					if len(names) >= 100000 {
						return errors.New("local source exceeds 100000 entries")
					}
					seen[name] = true
					names = append(names, name)
				}
				return nil
			}, doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())
			if err != nil {
				return content, fmt.Errorf("local include %q: %w", pattern, err)
			}
			if !matched {
				return content, fmt.Errorf("local include %q matched no files", pattern)
			}
		}
		var object cas.Ref
		object, err = m.importTree(ctx, root, names, s.SHA256)
		content.SHA256 = object.SHA256
		return content, err
	}
	done := plugin.Stage(ctx, "Reading local input", plugin.Detail(filepath.Base(s.Path)))
	defer func() { done(err) }()
	name := s.Path
	f, err := os.Open(name)
	if err != nil {
		return content, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return content, err
	}
	content.Tree, content.Mode = info.IsDir(), uint32(info.Mode().Perm())
	if content.Filename == "" {
		content.Filename = filepath.Base(s.Path)
	}
	if !validFilename(content.Filename) {
		return content, errors.New("input has no safe filename; set filename explicitly")
	}
	switch {
	case info.IsDir():
		tree, err := os.OpenRoot(name)
		if err != nil {
			return content, err
		}
		defer func() { _ = tree.Close() }()
		var object cas.Ref
		object, err = m.importTree(ctx, tree, nil, s.SHA256)
		content.SHA256 = object.SHA256
		return content, err
	case info.Mode().IsRegular():
		if err := archive.CheckXattrs(ctx, f); err != nil {
			return content, err
		}
		var object cas.Ref
		object, err = m.Store.Import(ctx, f, s.SHA256)
		content.SHA256 = object.SHA256
		return content, err
	}
	return content, errors.New("file source is not a regular file or directory")
}

func (m *Manager) importTree(ctx context.Context, root *os.Root, names []string, expected string) (cas.Ref, error) {
	return m.Store.Write(ctx, expected, func(w io.Writer) error { return archive.PackSelected(ctx, root, names, w) })
}
