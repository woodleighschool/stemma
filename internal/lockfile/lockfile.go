// Package lockfile separates update resolution from preparation of reviewed inputs.
package lockfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"go.yaml.in/yaml/v4"
)

// File contains digest-pinned recipes and plugin release indexes.
type File struct {
	Version int                      `yaml:"version" json:"version"`
	Recipes map[string]source.Entry  `yaml:"recipes" json:"recipes"`
	Plugins map[string]plugins.Entry `yaml:"plugins,omitempty" json:"plugins,omitempty"`
}

// Options controls lock consumption independently of cache use.
type Options struct {
	Frozen, Refresh, Ignore, Offline bool
	PluginsOnly                      bool
}

// Result reports acquisition separately from downstream metadata changes.
type Result struct {
	File      File
	Changed   bool
	CacheHits map[string]bool
}

// Lock serializes project operations and releases automatically after a process crash.
func Lock(ctx context.Context, root string) (func() error, error) {
	dir := filepath.Join(root, ".stemma")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, "project.lock"))
	ok, err := l.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	return l.Close, nil
}

// Load validates a versioned lockfile without resolving any providers.
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	if len(data) > 8<<20 {
		return File{}, errors.New("lockfile exceeds 8 MiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return f, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return f, errors.New("expected one lockfile document")
	}
	if f.Version != 1 || f.Recipes == nil {
		return f, errors.New("unsupported or incomplete lockfile; run stemma update")
	}
	return f, nil
}

// Prepare obtains exactly the required inputs, then replaces the lockfile atomically.
// A failed resolution never writes a partially updated lockfile.
func Prepare(ctx context.Context, p config.Project, m *source.Manager, opts Options) (Result, error) {
	result := Result{File: File{Version: 1, Recipes: map[string]source.Entry{}, Plugins: map[string]plugins.Entry{}}, CacheHits: map[string]bool{}}
	if opts.Frozen && (opts.Refresh || opts.Ignore) {
		return result, errors.New("frozen lockfile conflicts with refresh or no-lockfile")
	}
	if opts.Offline && (opts.Refresh || opts.Ignore) {
		return result, errors.New("offline requires a lockfile and cannot refresh")
	}
	filename := filepath.Join(m.Root, "stemma.lock.yaml")
	old := File{}
	if !opts.Ignore {
		loaded, err := Load(filename)
		if err != nil && (!errors.Is(err, os.ErrNotExist) || opts.Frozen || opts.Offline) {
			return result, fmt.Errorf("lockfile: %w", err)
		}
		old = loaded
	}
	resolve := func(s config.Source, previous source.Entry) (source.Entry, error) {
		current, err := m.Resolve(ctx, s)
		if err != nil {
			return current, err
		}
		current.ResolvedAt = previous.ResolvedAt
		if current.Artifact != previous.Artifact || current.ResolvedAt.IsZero() {
			// Checkout mtimes are not content identity. Capture one package timestamp
			// alongside the reviewed bytes and retain it on unchanged refreshes.
			current.ResolvedAt = time.Now().UTC().Truncate(time.Second)
		}
		return current, nil
	}
	acquire := func(key string, s config.Source, entry source.Entry) (source.Entry, error) {
		matches := entry.Source == s.Fingerprint() && !entry.ResolvedAt.IsZero()
		if s.Type == "file" || s.Type == "local" {
			if !matches && (opts.Frozen || opts.Offline) {
				return source.Entry{}, fmt.Errorf("%s is missing or stale in the lockfile; run stemma update", key)
			}
			current, err := resolve(s, entry)
			if err != nil {
				return current, err
			}
			unchanged := matches && current == entry
			if !unchanged && (opts.Frozen || opts.Offline) {
				return source.Entry{}, fmt.Errorf("%s local content changed; run stemma update", key)
			}
			result.CacheHits[key] = unchanged
			return current, nil
		}
		if matches && !opts.Refresh && !opts.Ignore {
			hit, err := m.Acquire(ctx, s, entry)
			result.CacheHits[key] = hit
			return entry, err
		}
		if opts.Frozen || opts.Offline {
			return source.Entry{}, fmt.Errorf("%s is missing or stale in the lockfile; run stemma update", key)
		}
		return resolve(s, entry)
	}
	if opts.PluginsOnly {
		result.File.Recipes = old.Recipes
		if result.File.Recipes == nil {
			result.File.Recipes = map[string]source.Entry{}
		}
	}
	for _, name := range names(p.Recipes) {
		if opts.PluginsOnly {
			break
		}
		entry, err := acquire(name, p.Recipes[name].Source, old.Recipes[name])
		if err != nil {
			return result, fmt.Errorf("%s: %w", name, err)
		}
		result.File.Recipes[name] = entry
	}
	pluginStore := plugins.New(m.Store, opts.Offline || m.Offline)
	for _, name := range names(p.Plugins) {
		image := p.Plugins[name].Image
		entry := old.Plugins[name]
		if opts.Ignore || entry.Validate(image) != nil || (opts.PluginsOnly && opts.Refresh) {
			if !opts.PluginsOnly || opts.Frozen || opts.Offline {
				return result, fmt.Errorf("plugin %s is not locked; run stemma plugins install (plugins never update implicitly)", name)
			}
			var err error
			entry, err = pluginStore.Resolve(ctx, image)
			if err != nil {
				return result, fmt.Errorf("plugin %s: %w", name, err)
			}
		}
		if _, err := pluginStore.Acquire(ctx, image, entry); err != nil {
			return result, fmt.Errorf("plugin %s: %w", name, err)
		}
		result.File.Plugins[name] = entry
	}
	result.Changed = config.Fingerprint(result.File) != config.Fingerprint(old)
	if opts.Frozen && result.Changed {
		return result, errors.New("lockfile contains stale entries; run stemma update")
	}
	if result.Changed && !opts.Ignore {
		data, err := yaml.Marshal(result.File)
		if err != nil {
			return result, err
		}
		if err := fileio.Write(filename, data, 0o644); err != nil {
			return result, err
		}
	}
	return result, nil
}

func names[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
