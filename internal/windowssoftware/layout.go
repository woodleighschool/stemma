package windowssoftware

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

func validateContent(content *Content) error {
	if content == nil {
		return nil
	}
	names := slices.Sorted(maps.Keys(content.Files))
	for i, name := range names {
		if !relative(name) {
			return fmt.Errorf("content.files has invalid path %q", name)
		}
		for _, previous := range names[:i] {
			left, right := strings.ToLower(previous), strings.ToLower(name)
			if left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/") {
				return fmt.Errorf("overlapping content paths %q and %q", previous, name)
			}
		}
	}
	return nil
}

type member struct {
	name      string
	directory bool
}
type layout map[string]member

func (l layout) add(name string, directory bool) error {
	if !relative(name) {
		return fmt.Errorf("invalid Windows content path %q", name)
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		key := strings.ToLower(parent)
		if prior, exists := l[key]; exists {
			if prior.name != parent || !prior.directory {
				return fmt.Errorf("content path %q conflicts with %q", name, prior.name)
			}
		} else {
			l[key] = member{parent, true}
		}
	}
	key := strings.ToLower(name)
	if prior, exists := l[key]; exists {
		return fmt.Errorf("content path %q overlaps %q", name, prior.name)
	}
	l[key] = member{name, directory}
	return nil
}

func (l layout) input(ctx context.Context, artifact plugin.Artifact, prefix string) error {
	if !filepath.IsAbs(artifact.Path) {
		return errors.New("content inputs require absolute leased paths")
	}
	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return err
	}
	if info.IsDir() != artifact.Tree || !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("content input file type differs from its leased artifact")
	}
	if prefix != "" {
		if err := l.add(prefix, artifact.Tree); err != nil {
			return err
		}
	}
	if !artifact.Tree {
		return nil
	}
	return filepath.WalkDir(artifact.Path, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if filename == artifact.Path {
			return nil
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported Windows content type at %s", filename)
		}
		relativeName, err := filepath.Rel(artifact.Path, filename)
		if err != nil {
			return err
		}
		return l.add(path.Join(prefix, filepath.ToSlash(relativeName)), entry.IsDir())
	})
}

func validateLayout(ctx context.Context, source plugin.Artifact, content *Content, inputs map[string]plugin.Artifact) error {
	members := layout{}
	prefix := source.Filename
	if source.Tree {
		prefix = ""
	}
	if err := members.input(ctx, source, prefix); err != nil {
		return err
	}
	if content == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(content.Files)) {
		if err := members.input(ctx, inputs["file:"+name], name); err != nil {
			return fmt.Errorf("content file %s: %w", name, err)
		}
	}
	return nil
}
