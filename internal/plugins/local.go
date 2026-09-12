package plugins

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Load snapshots local code or acquires an installed release. Frozen local loads
// check the observation before any plugin code executes.
func (s *Store) Load(ctx context.Context, root string, declaration config.Plugin, previous Entry, frozen bool) (Bundle, Entry, error) {
	if err := declaration.Validate(); err != nil {
		return Bundle{}, Entry{}, err
	}
	if declaration.Path == "" {
		bundle, err := s.Acquire(ctx, declaration.Image, previous)
		return bundle, previous, err
	}
	path := declaration.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	manager := source.New(s.cache, filepath.Dir(path), true)
	current, err := manager.Resolve(ctx, plugin.Input{Resolver: "file", Config: map[string]any{"path": filepath.Base(path)}})
	if err != nil {
		return Bundle{}, Entry{}, err
	}
	if !current.Content.Tree && declaration.Entrypoint != "" {
		return Bundle{}, Entry{}, errors.New("entrypoint requires a directory; path already selects an executable")
	}
	if previous.Local != nil && previous.Local.Content == current.Content {
		current.ResolvedAt = previous.Local.ResolvedAt
	}
	entry := Entry{Path: declaration.Path, Entrypoint: declaration.Entrypoint, Local: &current}
	if frozen && (previous.Image != "" || previous.Digest != "" || previous.Size != 0 || previous.Path != entry.Path || previous.Entrypoint != entry.Entrypoint || previous.Local == nil || !current.Equal(*previous.Local)) {
		return Bundle{}, Entry{}, errors.New("local plugin is missing or changed in the lockfile; run stemma prepare")
	}
	name := declaration.Entrypoint
	if name == "" {
		name = "plugin"
		if s.platform.OS == "windows" {
			name += ".exe"
		}
		if !current.Content.Tree {
			name = current.Content.Filename
		}
	}
	identity := config.Fingerprint(struct {
		Content    source.Content
		Entrypoint string
	}{current.Content, name})
	bundle := Bundle{Manifest: identity, Artifact: current.Content.Artifact, Local: &current.Content, Entrypoint: name}
	return bundle, entry, nil
}

// Install resolves registry images explicitly; local sources follow their files.
func (s *Store) Install(ctx context.Context, root string, declarations map[string]config.Plugin, previous map[string]Entry, refresh bool) (map[string]Entry, error) {
	entries := make(map[string]Entry, len(declarations))
	for _, name := range slices.Sorted(maps.Keys(declarations)) {
		declaration := declarations[name]
		entry := previous[name]
		if declaration.Image != "" && (refresh || entry.Validate(declaration.Image) != nil) {
			var err error
			entry, err = s.Resolve(ctx, declaration.Image)
			if err != nil {
				return nil, fmt.Errorf("plugin %s: %w", name, err)
			}
		}
		_, entry, err := s.Load(ctx, root, declaration, entry, false)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", name, err)
		}
		entries[name] = entry
	}
	return entries, nil
}
