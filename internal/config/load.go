package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Fragment contains the recipes belonging to one software family. Project
// mechanics and component defaults are declared only in the root project file.
type Fragment struct {
	Version int               `yaml:"version" json:"version" jsonschema:"enum=1"`
	Recipes map[string]Recipe `yaml:"recipes" json:"recipes"`
}

// Load resolves inline recipes and imported software-family files within the
// project directory. It does not read credentials or use the network.
func Load(filename string) (Project, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Project{}, err
	}
	var p Project
	document, err := parseDocument(data, &p)
	if err != nil {
		return p, err
	}
	components, _ := document["components"].(map[string]any)
	recipes, _ := document["recipes"].(map[string]any)
	p.Recipes = map[string]Recipe{}
	if err := addRecipes(&p, recipes, components, "."); err != nil {
		return p, err
	}
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return p, err
	}
	defer func() { _ = root.Close() }()
	seen := map[string]bool{}
	for _, pattern := range p.Imports {
		if !safeRelative(pattern) || !doublestar.ValidatePattern(pattern) {
			return p, fmt.Errorf("invalid import pattern %q", pattern)
		}
		matches, err := doublestar.Glob(root.FS(), pattern, doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())
		if err != nil {
			return p, fmt.Errorf("import %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return p, fmt.Errorf("import pattern %q matched no files", pattern)
		}
		for _, name := range matches {
			if seen[name] {
				continue
			}
			seen[name] = true
			if name == filepath.Base(filename) {
				return p, errors.New("project cannot import itself")
			}
			data, err := fs.ReadFile(root.FS(), name)
			if err != nil {
				return p, fmt.Errorf("import %s: %w", name, err)
			}
			var fragment Fragment
			document, err := parseDocument(data, &fragment)
			if err != nil {
				return p, fmt.Errorf("import %s: %w", name, err)
			}
			if fragment.Version != 1 || len(fragment.Recipes) == 0 {
				return p, fmt.Errorf("import %s requires version 1 and recipes", name)
			}
			recipes, _ := document["recipes"].(map[string]any)
			if err := addRecipes(&p, recipes, components, path.Dir(name)); err != nil {
				return p, fmt.Errorf("import %s: %w", name, err)
			}
		}
	}
	return p, p.Validate()
}

// FindRoot climbs from a directory past software-family fragments to the nearest
// project file. Malformed files are reported rather than silently skipped.
func FindRoot(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	for {
		filename := filepath.Join(dir, "stemma.yaml")
		data, err := os.ReadFile(filename)
		if err == nil {
			var project Project
			document, err := parseDocument(data, &project)
			if err != nil {
				return "", fmt.Errorf("%s: %w", filename, err)
			}
			if _, present := document["project"]; present {
				return dir, nil
			}
			var fragment Fragment
			if _, err := parseDocument(data, &fragment); err != nil || fragment.Version != 1 || len(fragment.Recipes) == 0 {
				return "", fmt.Errorf("%s is neither a project nor a valid software-family fragment", filename)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no root stemma.yaml with a project identity found")
		}
		dir = parent
	}
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
