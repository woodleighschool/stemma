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
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

// File pins resource inputs and executable plugin content.
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
func Lock(ctx context.Context, root string) (unlock func() error, err error) {
	dir := filepath.Join(root, ".stemma")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, "project.lock"))
	done := plugin.Stage(ctx, "Acquiring project lock")
	defer func() { done(err) }()
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

// Update stages input observations until Commit replaces the lockfile atomically.
// Callers hold the project lock for its lifetime.
type Update struct {
	result   Result
	old      File
	filename string
	opts     Options
	inputs   map[string]map[string]plugin.Input
	acquire  func(context.Context, plugin.Input, source.Entry) (source.Entry, bool, error)
}

// Begin loads reviewed inputs without acquiring resource content.
func Begin(ctx context.Context, root string, inputs map[string]map[string]plugin.Input, pluginEntries map[string]plugins.Entry, m *source.Manager, opts Options) (*Update, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := Result{File: File{Version: 2, Inputs: map[string]map[string]source.Entry{}, Plugins: map[string]plugins.Entry{}}, CacheHits: map[string]map[string]bool{}}
	opts.Offline = opts.Offline || m.Offline
	manager := *m
	manager.Offline = opts.Offline
	m = &manager
	if opts.Frozen && (opts.Refresh || opts.Ignore) {
		return nil, errors.New("frozen lockfile conflicts with refresh or no-lockfile")
	}
	if opts.Offline && (opts.Refresh || opts.Ignore) {
		return nil, errors.New("offline requires a lockfile and cannot refresh")
	}
	filename := filepath.Join(root, "stemma.lock.yaml")
	old := File{}
	requiresLock := len(pluginEntries) != 0
	for _, named := range inputs {
		requiresLock = requiresLock || len(named) != 0
	}
	if !opts.Ignore {
		loaded, err := Load(filename)
		if err != nil && (!errors.Is(err, os.ErrNotExist) || requiresLock && (opts.Frozen || opts.Offline)) {
			return nil, fmt.Errorf("lockfile: %w", err)
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
	resolve := func(ctx context.Context, input plugin.Input, previous source.Entry) (source.Entry, error) {
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
	acquire := func(ctx context.Context, input plugin.Input, entry source.Entry) (source.Entry, bool, error) {
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
			current, err := resolve(ctx, input, entry)
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
		current, err := resolve(ctx, input, entry)
		return current, false, err
	}
	if opts.PluginsOnly {
		result.File.Inputs = old.Inputs
		if result.File.Inputs == nil {
			result.File.Inputs = map[string]map[string]source.Entry{}
		}
	}
	result.File.Plugins = pluginEntries
	return &Update{result: result, old: old, filename: filename, opts: opts, inputs: inputs, acquire: acquire}, nil
}

// Acquire obtains one resource's inputs. Repeated calls reuse the same observation.
func (u *Update) Acquire(ctx context.Context, resource string) (map[string]source.Entry, map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if entries, ok := u.result.File.Inputs[resource]; ok {
		return entries, u.result.CacheHits[resource], nil
	}
	inputs, ok := u.inputs[resource]
	if !ok || resource == "" {
		return nil, nil, fmt.Errorf("unknown input resource %q", resource)
	}
	entries := map[string]source.Entry{}
	hits := map[string]bool{}
	for _, name := range names(inputs) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if name == "" {
			return nil, nil, errors.New("input lock has an empty input name")
		}
		label := resource
		if parts := strings.Split(resource, "/"); len(parts) >= 2 {
			label = strings.Join(parts[len(parts)-2:], "/")
		}
		ctx := plugin.WithLogger(ctx, plugin.Logger(ctx).With("resource", label, "input", name))
		done := plugin.Stage(ctx, "Acquiring input")
		entry, hit, err := u.acquire(ctx, inputs[name], u.old.Inputs[resource][name])
		done(err, "cached", hit)
		if err != nil {
			return nil, nil, fmt.Errorf("%s input %s: %w", resource, name, err)
		}
		entries[name], hits[name] = entry, hit
	}
	if len(entries) > 0 {
		u.result.File.Inputs[resource] = entries
		u.result.CacheHits[resource] = hits
	}
	return entries, hits, nil
}

// Commit replaces the lockfile only after every selected input was acquired.
func (u *Update) Commit(ctx context.Context) (Result, error) {
	result, old, opts, filename := u.result, u.old, u.opts, u.filename
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !opts.PluginsOnly {
		for resource, inputs := range u.inputs {
			if len(result.File.Inputs[resource]) != len(inputs) {
				return result, fmt.Errorf("%s inputs were not acquired", resource)
			}
		}
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
		var data bytes.Buffer
		encoder := yaml.NewEncoder(&data)
		encoder.SetIndent(2)
		if err := encoder.Encode(result.File); err != nil {
			return result, err
		}
		if err := encoder.Close(); err != nil {
			return result, err
		}
		if err := fileio.Write(filename, data.Bytes(), 0o644); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Prepare acquires all selected inputs and commits their observations atomically.
func Prepare(ctx context.Context, root string, inputs map[string]map[string]plugin.Input, pluginEntries map[string]plugins.Entry, m *source.Manager, opts Options) (Result, error) {
	update, err := Begin(ctx, root, inputs, pluginEntries, m, opts)
	if err != nil {
		return Result{}, err
	}
	if !opts.PluginsOnly {
		for _, resource := range names(inputs) {
			if _, _, err := update.Acquire(ctx, resource); err != nil {
				return Result{}, err
			}
		}
	}
	return update.Commit(ctx)
}

func names[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
