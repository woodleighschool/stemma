package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

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
