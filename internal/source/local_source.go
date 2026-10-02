package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/plugin"
)

type fileConfig struct {
	Path     string `json:"path" jsonschema_description:"Exact file or directory path. Relative paths resolve from the resource file; absolute paths select a host location."`
	Filename string `json:"filename,omitempty" jsonschema_description:"Optional input basename override. Defaults to the selected path's basename. Independent of publication naming."`
	SHA256   string `json:"sha256,omitempty" jsonschema_description:"Optional expected SHA-256 content digest, as 64 lowercase hexadecimal characters."`
}

type localConfig struct {
	Include  []string `json:"include" jsonschema_description:"Local tree globs using doublestar semantics. Each pattern must match at least one entry."`
	Base     string   `json:"base,omitempty" jsonschema_description:"Local tree root, relative to the resource file. Defaults to its directory."`
	Filename string   `json:"filename,omitempty" jsonschema_description:"Optional input basename override. Defaults to the selected base's basename. Independent of publication naming."`
	SHA256   string   `json:"sha256,omitempty" jsonschema_description:"Optional expected SHA-256 content digest, as 64 lowercase hexadecimal characters."`
}

func fileInput(input plugin.Input) (fileConfig, error) {
	s, err := configFor[fileConfig](input)
	if err != nil {
		return s, err
	}
	if s.Path == "" {
		return s, errors.New("file source requires a file or directory path")
	}
	if err := validateContent(s.Filename, s.SHA256); err != nil {
		return s, err
	}
	s.Path, err = resolvePath(input.Base, s.Path)
	return s, err
}

func localInput(input plugin.Input) (localConfig, error) {
	s, err := configFor[localConfig](input)
	if err != nil {
		return s, err
	}
	if len(s.Include) == 0 {
		return s, errors.New("local source requires include patterns relative to its software-family file")
	}
	for _, pattern := range s.Include {
		if !safeRelative(pattern) || !doublestar.ValidatePattern(pattern) {
			return s, fmt.Errorf("invalid local include pattern %q", pattern)
		}
	}
	if err := validateContent(s.Filename, s.SHA256); err != nil {
		return s, err
	}
	s.Base, err = projectPath(input.Base, s.Base)
	return s, err
}

func (m *Manager) fileResolver() Resolver {
	resolver := resolverFor(fileInput,
		func(context.Context, fileConfig) (Discovery, error) {
			return Discovery{Observation: json.RawMessage(`{}`)}, nil
		},
		func(_ context.Context, s fileConfig, observation json.RawMessage) (Acquisition, error) {
			if err := decode(observation, &struct{}{}); err != nil {
				return Acquisition{}, fmt.Errorf("file observation: %w", err)
			}
			name := filepath.FromSlash(s.Path)
			if !absolutePath(s.Path) {
				name = filepath.Join(m.Root, name)
			}
			return Acquisition{File: &fileRequest{Path: name, Filename: s.Filename, SHA256: s.SHA256}}, nil
		})
	resolver.Local = true
	resolver.Changed = func(input plugin.Input, changed []string) (bool, error) {
		// Environment paths can refer to inputs absent from either checkout.
		if expression.Has(input.Config["path"]) {
			return false, nil
		}
		name, _ := input.Config["path"].(string)
		base, err := resolvePath(input.Base, name)
		if err != nil {
			return false, err
		}
		return pathChanged(base, changed), nil
	}
	return resolver
}

func (m *Manager) localResolver() Resolver {
	resolver := resolverFor(localInput,
		func(context.Context, localConfig) (Discovery, error) {
			return Discovery{Observation: json.RawMessage(`{}`)}, nil
		},
		func(_ context.Context, s localConfig, observation json.RawMessage) (Acquisition, error) {
			if err := decode(observation, &struct{}{}); err != nil {
				return Acquisition{}, fmt.Errorf("local observation: %w", err)
			}
			return Acquisition{File: &fileRequest{Path: s.Base, Include: s.Include, Filename: s.Filename, SHA256: s.SHA256}}, nil
		})
	resolver.Local = true
	resolver.Changed = func(input plugin.Input, changed []string) (bool, error) {
		if expression.Has(input.Config["base"]) {
			return false, nil
		}
		name, _ := input.Config["base"].(string)
		base, err := projectPath(input.Base, name)
		if err != nil {
			return false, err
		}
		return pathChanged(base, changed), nil
	}
	return resolver
}

// Tree dependencies are conservative: directory edits select the source even
// when an include pattern later excludes the changed file.
func pathChanged(base string, changed []string) bool {
	if absolutePath(base) {
		return false
	}
	for _, name := range changed {
		if base == "." && filepath.IsLocal(filepath.FromSlash(name)) || name == base || strings.HasPrefix(name, base+"/") || strings.HasPrefix(base, name+"/") {
			return true
		}
	}
	return false
}
