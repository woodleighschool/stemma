package archive

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Leaves names the files and symlinks an extraction keeps, as slash-separated
// paths relative to the extracted root. A leaf's directory is literal and its
// last element may be a [path.Match] pattern. Extractors create and traverse
// the directories leading to a leaf themselves. A nil Leaves keeps everything.
type Leaves []string

// Literal escapes name so that, as the last element of a leaf, it matches only itself.
func Literal(name string) string {
	return leafEscaper.Replace(name)
}

var leafEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)

// Validate requires every leaf to be a relative path below the root with a
// literal directory and a well-formed last element.
func (l Leaves) Validate() error {
	for _, leaf := range l {
		if leaf == "." || !fs.ValidPath(leaf) || strings.ContainsAny(path.Dir(leaf), `\*?[`) {
			return fmt.Errorf("extraction leaf %q must be a relative path with a literal directory", leaf)
		}
		if _, err := path.Match(path.Base(leaf), ""); err != nil {
			return fmt.Errorf("extraction leaf %q: %w", leaf, err)
		}
	}
	return nil
}

// Keeps reports whether the file or symlink name is kept.
func (l Leaves) Keeps(name string) bool {
	if l == nil {
		return true
	}
	dir, base := path.Dir(name), path.Base(name)
	for _, leaf := range l {
		if path.Dir(leaf) != dir {
			continue
		}
		if matched, _ := path.Match(path.Base(leaf), base); matched {
			return true
		}
	}
	return false
}

// Leads reports whether the directory dir is the root or lies on the way to a leaf.
func (l Leaves) Leads(dir string) bool {
	if l == nil || dir == "." {
		return true
	}
	for _, leaf := range l {
		if parent := path.Dir(leaf); parent == dir || strings.HasPrefix(parent, dir+"/") {
			return true
		}
	}
	return false
}
