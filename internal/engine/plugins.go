package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// ErrPluginsFailed reports that a plugin report holds a plugin that did not
// load or offers operations this Stemma cannot use; the report says which.
var ErrPluginsFailed = errors.New("plugins failed")

// PluginReport describes a declared plugin: the code its declaration and lock
// entry select, what it offers, and what it cannot.
type PluginReport struct {
	Name  string `json:"name"`
	Image string `json:"image,omitempty"`
	Path  string `json:"path,omitempty"`
	// Digest names the code: the platform index of an image or the content of
	// local files. Locked reports that it comes from the lockfile rather than
	// the declaration.
	Digest string `json:"digest,omitempty"`
	Locked bool   `json:"locked,omitempty"`
	// Before is the digest the lockfile held before an update.
	Before      string               `json:"before,omitempty"`
	Version     string               `json:"version,omitempty"`
	Revision    string               `json:"revision,omitempty"`
	Interfaces  map[string]int       `json:"interfaces,omitempty"`
	Platforms   []string             `json:"platforms,omitempty"`
	Operations  []PluginOperation    `json:"operations,omitempty"`
	Unavailable []plugin.Unavailable `json:"unavailable,omitempty"`
	Error       string               `json:"error,omitempty"`
}

// PluginOperation identifies an operation a plugin offers.
type PluginOperation struct {
	Name     string               `json:"name"`
	Kind     string               `json:"kind"`
	Resource *plugin.ResourceKind `json:"resource,omitempty"`
}

// PluginUpdate reports what plugins update locked. Removed names the entries
// of plugins no longer declared.
type PluginUpdate struct {
	Plugins     []PluginReport `json:"plugins"`
	Removed     []string       `json:"removed,omitempty"`
	LockChanged bool           `json:"lock_changed"`
}

// ListPlugins loads every declared plugin from its lock entry, as runs do,
// and reports each one. It returns ErrPluginsFailed when a report holds a
// failure.
func ListPlugins(ctx context.Context, opts Options) ([]PluginReport, error) {
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return nil, err
	}
	locked, err := lockfile.Load(lockfile.Filename(root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var reports []PluginReport
	err = withPluginWorkspace(ctx, opts.CacheDir, func(store *cas.Store, work string) error {
		var err error
		reports, _, err = describePlugins(ctx, p, plugins.New(store, opts.Lock.Offline), root, work, slices.Sorted(maps.Keys(p.Plugins)), locked.Plugins, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reports, pluginsFailed(reports)
}

// UpdatePlugins locks the named plugins, or every declared plugin, to what
// their declarations select now: a tag is resolved again and a local path
// snapshotted. Each plugin is described first; one that does not load keeps
// its entry. Entries of plugins no longer declared are removed. It returns
// ErrPluginsFailed when a report holds a failure.
func UpdatePlugins(ctx context.Context, opts Options, names []string) (update PluginUpdate, err error) {
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return update, err
	}
	for _, name := range names {
		if _, declared := p.Plugins[name]; !declared {
			return update, fmt.Errorf("plugin %s is not declared", name)
		}
	}
	if len(names) == 0 {
		names = slices.Collect(maps.Keys(p.Plugins))
	}
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return update, err
	}
	unlock, err := lockfile.Lock(ctx, root)
	if err != nil {
		return update, err
	}
	defer func() { _ = unlock() }()
	locked, err := lockfile.Load(lockfile.Filename(root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return update, err
	}
	err = withPluginWorkspace(ctx, opts.CacheDir, func(store *cas.Store, work string) error {
		reports, resolved, err := describePlugins(ctx, p, plugins.New(store, false), root, work, names, nil, true)
		if err != nil {
			return err
		}
		entries := map[string]plugins.Entry{}
		for name, entry := range locked.Plugins {
			if _, declared := p.Plugins[name]; declared {
				entries[name] = entry
			}
		}
		for i, report := range reports {
			reports[i].Before = locked.Plugins[report.Name].Digest
			if report.Error == "" {
				delete(entries, report.Name)
				if entry, ok := resolved[report.Name]; ok {
					entries[report.Name] = entry
				}
			}
		}
		update.Plugins = reports
		result, err := lockfile.Prepare(ctx, root, nil, entries, source.New(store, root, false), lockfile.Options{PluginsOnly: true})
		if err != nil {
			return err
		}
		update.LockChanged = result.Changed
		for _, change := range result.Plugins {
			if _, declared := p.Plugins[change.Name]; !declared {
				update.Removed = append(update.Removed, change.Name)
			}
		}
		return nil
	})
	if err != nil {
		return update, err
	}
	return update, pluginsFailed(update.Plugins)
}

// withPluginWorkspace leases the cache and gives plugins a workspace to
// materialize in for the duration of run.
func withPluginWorkspace(ctx context.Context, cacheDir string, run func(*cas.Store, string) error) error {
	store, err := cas.Open(cacheDir)
	if err != nil {
		return err
	}
	release, err := store.Lease(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	work, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "plugins-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	return run(store, work)
}

// describePlugins loads the named plugins and reports each as a run would
// register it, with the lock entries of those that loaded.
func describePlugins(ctx context.Context, p config.Project, store *plugins.Store, root, work string, names []string, previous map[string]plugins.Entry, resolve bool) ([]PluginReport, map[string]plugins.Entry, error) {
	ops, err := builtins(nil)
	if err != nil {
		return nil, nil, err
	}
	reports := make([]PluginReport, 0, len(names))
	entries := map[string]plugins.Entry{}
	for _, name := range names {
		declaration := p.Plugins[name]
		loaded := loadPlugin(ctx, store, root, work, name, declaration, previous[name], resolve)
		report := PluginReport{Name: name, Image: declaration.Image, Path: declaration.Path, Version: loaded.description.Version, Revision: loaded.description.Revision, Interfaces: loaded.description.Interfaces, Platforms: loaded.bundle.Platforms, Unavailable: loaded.description.Unavailable}
		report.Digest = plugins.Pin(declaration, loaded.entry)
		report.Locked = loaded.entry.Digest != ""
		for _, operation := range loaded.description.Operations {
			report.Operations = append(report.Operations, PluginOperation{Name: operation.Name, Kind: operation.Kind, Resource: operation.Resource})
		}
		if ops.add(loaded) {
			if loaded.entry != (plugins.Entry{}) {
				entries[name] = loaded.entry
			}
		} else {
			report.Error = ops.failed[name].Error()
		}
		reports = append(reports, report)
	}
	return reports, entries, nil
}

func pluginsFailed(reports []PluginReport) error {
	for _, report := range reports {
		if report.Error != "" || len(report.Unavailable) > 0 {
			return ErrPluginsFailed
		}
	}
	return nil
}

// loadedPlugin is one declared plugin as a run loads it: the lock entry that
// selects its code and what its describe answer offers, or why it did not load.
type loadedPlugin struct {
	name        string
	entry       plugins.Entry
	bundle      plugins.Bundle
	executable  string
	description plugin.Description
	err         error
}

// loadPlugin selects a plugin's code, materializes it in work and describes it.
func loadPlugin(ctx context.Context, store *plugins.Store, root, work, name string, declaration config.Plugin, previous plugins.Entry, frozen bool) (loaded loadedPlugin) {
	loaded.name = name
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("plugin", name))
	done := plugin.Stage(ctx, "Loading plugin")
	defer func() { done(loaded.err, plugin.Detail(loaded.description.Version)) }()
	loaded.bundle, loaded.entry, loaded.err = store.Load(ctx, root, declaration, previous, frozen)
	if loaded.err != nil {
		return loaded
	}
	loaded.executable, loaded.err = store.Materialize(ctx, loaded.bundle, filepath.Join(work, name))
	if loaded.err != nil {
		return loaded
	}
	loaded.description, loaded.err = plugin.Describe(ctx, loaded.executable)
	return loaded
}

// add registers the operations a loaded plugin offers and records why the rest
// are unavailable. It reports whether the plugin loaded.
func (o *operations) add(loaded loadedPlugin) bool {
	err := loaded.err
	if err == nil {
		err = o.register(loaded)
	}
	if err != nil {
		o.failed[loaded.name] = err
		return false
	}
	for _, operation := range loaded.description.Unavailable {
		err := fmt.Errorf("%s is unavailable: plugin %s %s", unavailableName(operation), loaded.name, operation.Reason)
		o.unavailable[operation.Name] = err
		if operation.Resource != nil {
			o.unavailableKinds[*operation.Resource] = err
		}
	}
	return true
}

// register adds all of a plugin's usable operations, or none when one would
// collide with an operation already provided.
func (o *operations) register(loaded loadedPlugin) error {
	names := map[string]bool{}
	kinds := map[plugin.ResourceKind]bool{}
	for _, operation := range o.registry.Descriptor().Operations {
		names[operation.Name] = true
		if operation.Resource != nil {
			kinds[*operation.Resource] = true
		}
	}
	for name := range o.unavailable {
		names[name] = true
	}
	for _, operation := range loaded.description.Operations {
		switch {
		case operation.Resolver != nil && source.NativeResolver(operation.Name):
			return fmt.Errorf("resolver %q is built in", operation.Name)
		case names[operation.Name]:
			return fmt.Errorf("operation %q is already provided", operation.Name)
		case operation.Resource != nil && kinds[*operation.Resource]:
			return fmt.Errorf("resource kind %s/%s is already registered", operation.Resource.APIVersion, operation.Resource.Kind)
		}
		names[operation.Name] = true
		if operation.Resource != nil {
			kinds[*operation.Resource] = true
		}
	}
	for _, operation := range loaded.description.Unavailable {
		if names[operation.Name] {
			return fmt.Errorf("operation %q is already provided", operation.Name)
		}
	}
	identity := config.Fingerprint(struct {
		Manifest   string
		Descriptor plugin.Descriptor
	}{loaded.bundle.Manifest, loaded.description.Descriptor})
	executable := loaded.executable
	for _, operation := range loaded.description.Operations {
		if err := o.registry.Register(operation, func(ctx context.Context, request plugin.Request) (plugin.Response, error) {
			return plugin.Run(ctx, executable, request)
		}); err != nil {
			return err
		}
		o.identity[operation.Name] = identity
	}
	return nil
}

func unavailableName(operation plugin.Unavailable) string {
	switch {
	case operation.Resource != nil:
		return "resource kind " + operation.Resource.APIVersion + "/" + operation.Resource.Kind
	case operation.Kind == "resolve":
		return "resolver " + operation.Name
	}
	return "operation " + operation.Name
}

// missing explains a lookup no registered operation answers, naming the
// plugins that did not load and so might have answered it.
func (o *operations) missing(message string) error {
	for _, name := range slices.Sorted(maps.Keys(o.failed)) {
		message += fmt.Sprintf("; plugin %s did not load: %v", name, o.failed[name])
	}
	return errors.New(message)
}

// complete reports every plugin that did not load and every operation offered
// at another interface version, for commands that describe all operations.
func (o *operations) complete() error {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(o.failed)) {
		problems = append(problems, fmt.Sprintf("plugin %s: %v", name, o.failed[name]))
	}
	for _, name := range slices.Sorted(maps.Keys(o.unavailable)) {
		problems = append(problems, o.unavailable[name].Error())
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "\n"))
}
