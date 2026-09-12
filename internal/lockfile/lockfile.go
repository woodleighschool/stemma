// Package lockfile separates update resolution from preparation of reviewed inputs.
package lockfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

// File pins each resource's named inputs and explicitly installed plugin indexes.
type File struct {
	Version int                                `yaml:"version" json:"version"`
	Inputs  map[string]map[string]source.Entry `yaml:"inputs" json:"inputs"`
	Plugins map[string]plugins.Entry           `yaml:"plugins,omitempty" json:"plugins,omitempty"`
}

// Options controls lock consumption independently of cache use.
type Options struct {
	Frozen, Refresh, Ignore, Offline bool
	PluginsOnly                      bool
	PreserveUnselected               bool
}

// Result reports acquisition separately from downstream metadata changes.
type Result struct {
	File      File
	Changed   bool
	CacheHits map[string]map[string]bool
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
	if f.Version != 2 || f.Inputs == nil {
		return f, errors.New("unsupported or incomplete lockfile; run stemma update")
	}
	for resource, inputs := range f.Inputs {
		if resource == "" || len(inputs) == 0 {
			return f, errors.New("input lock requires a resource and named inputs")
		}
		for name, entry := range inputs {
			if name == "" {
				return f, errors.New("input lock has an empty input name")
			}
			if err := entry.Validate(); err != nil {
				return f, fmt.Errorf("%s input %s: %w", resource, name, err)
			}
		}
	}
	return f, nil
}

// Prepare obtains exactly the required inputs, then replaces the lockfile atomically.
// A failed resolution never writes a partially updated lockfile.
func Prepare(ctx context.Context, root string, inputs map[string]map[string]plugin.Input, pluginImages map[string]string, m *source.Manager, opts Options) (Result, error) {
	result := Result{File: File{Version: 2, Inputs: map[string]map[string]source.Entry{}, Plugins: map[string]plugins.Entry{}}, CacheHits: map[string]map[string]bool{}}
	opts.Offline = opts.Offline || m.Offline
	manager := *m
	manager.Offline = opts.Offline
	m = &manager
	if opts.Frozen && (opts.Refresh || opts.Ignore) {
		return result, errors.New("frozen lockfile conflicts with refresh or no-lockfile")
	}
	if opts.Offline && (opts.Refresh || opts.Ignore) {
		return result, errors.New("offline requires a lockfile and cannot refresh")
	}
	filename := filepath.Join(root, "stemma.lock.yaml")
	old := File{}
	requiresLock := len(pluginImages) != 0
	for _, named := range inputs {
		requiresLock = requiresLock || len(named) != 0
	}
	if !opts.Ignore {
		loaded, err := Load(filename)
		if err != nil && (!errors.Is(err, os.ErrNotExist) || requiresLock && (opts.Frozen || opts.Offline)) {
			return result, fmt.Errorf("lockfile: %w", err)
		}
		old = loaded
	}
	if opts.PreserveUnselected {
		for resource, entries := range old.Inputs {
			if _, selected := inputs[resource]; !selected {
				result.File.Inputs[resource] = entries
			}
		}
	}
	resolved := map[string]source.Entry{}
	resolve := func(input plugin.Input, previous source.Entry) (source.Entry, error) {
		version, declaration, err := m.Declaration(input)
		if err != nil {
			return source.Entry{}, err
		}
		key := input.Resolver + "\x00" + version + "\x00" + declaration
		current, ok := resolved[key]
		if !ok {
			current, err = m.Resolve(ctx, input)
			if err != nil {
				return current, err
			}
			resolved[key] = current
		}
		// Keep the reviewed timestamp whenever immutable bytes stay the same.
		if current.Content.Artifact == previous.Content.Artifact && !previous.ResolvedAt.IsZero() {
			current.ResolvedAt = previous.ResolvedAt
		}
		return current, nil
	}
	acquire := func(input plugin.Input, entry source.Entry) (source.Entry, bool, error) {
		version, declaration, err := m.Declaration(input)
		if err != nil {
			return source.Entry{}, false, err
		}
		matches := entry.Version == 1 && entry.Resolver == input.Resolver && entry.ResolverVersion == version && entry.Declaration == declaration && !entry.ResolvedAt.IsZero()
		if m.IsLocal(input.Resolver) {
			if !matches && (opts.Frozen || opts.Offline) {
				return source.Entry{}, false, errors.New("input is missing or stale in the lockfile; run stemma update")
			}
			cached := matches && m.Store.Verify(ctx, entry.Content.Artifact) == nil
			current, err := resolve(input, entry)
			if err != nil {
				return current, false, err
			}
			unchanged := matches && current.Equal(entry)
			if !unchanged && (opts.Frozen || opts.Offline) {
				return source.Entry{}, false, errors.New("local input content changed; run stemma update")
			}
			return current, unchanged && cached, nil
		}
		if matches && !opts.Refresh && !opts.Ignore {
			hit, err := m.FetchLocked(ctx, input, entry)
			return entry, hit, err
		}
		if opts.Frozen || opts.Offline {
			return source.Entry{}, false, errors.New("input is missing or stale in the lockfile; run stemma update")
		}
		current, err := resolve(input, entry)
		return current, false, err
	}
	if opts.PluginsOnly {
		result.File.Inputs = old.Inputs
		if result.File.Inputs == nil {
			result.File.Inputs = map[string]map[string]source.Entry{}
		}
	} else {
		for _, resource := range names(inputs) {
			if len(inputs[resource]) == 0 {
				continue
			}
			if resource == "" {
				return result, errors.New("inputs require a resource identity")
			}
			result.File.Inputs[resource] = map[string]source.Entry{}
			result.CacheHits[resource] = map[string]bool{}
			for _, name := range names(inputs[resource]) {
				if name == "" {
					return result, errors.New("inputs require a name")
				}
				entry, hit, err := acquire(inputs[resource][name], old.Inputs[resource][name])
				if err != nil {
					return result, fmt.Errorf("%s input %s: %w", resource, name, err)
				}
				result.File.Inputs[resource][name] = entry
				result.CacheHits[resource][name] = hit
			}
		}
	}
	pluginStore := plugins.New(m.Store, opts.Offline || m.Offline)
	for _, name := range names(pluginImages) {
		image := pluginImages[name]
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
	empty := len(result.File.Inputs) == 0 && len(result.File.Plugins) == 0
	if empty && old.Version == 0 {
		return result, nil
	}
	before, err := json.Marshal(old)
	if err != nil {
		return result, err
	}
	after, err := json.Marshal(result.File)
	if err != nil {
		return result, err
	}
	result.Changed = !bytes.Equal(before, after)
	if opts.Frozen && result.Changed {
		return result, errors.New("lockfile contains stale entries; run stemma update")
	}
	if result.Changed && !opts.Ignore {
		if empty {
			return result, os.Remove(filename)
		}
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
