package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/internal/intune"
	"github.com/woodleighschool/stemma/internal/jamf"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/munkirepo"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Catalog describes built-in and installed plugin operations without acquiring
// software inputs or contacting destinations. Trusted plugin discovery executes code.
func Catalog(ctx context.Context, opts Options) (result plugin.Descriptor, err error) {
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return plugin.Descriptor{}, err
	}
	done := plugin.Stage(ctx, "Loading operation contracts")
	defer func() { done(err) }()
	ops, cleanup, err := projectOperations(ctx, p, opts)
	if err != nil {
		return plugin.Descriptor{}, err
	}
	defer cleanup()
	if err := ops.complete(); err != nil {
		return plugin.Descriptor{}, err
	}
	return ops.registry.Descriptor(), nil
}

// ValidateProject checks the catalog as written: declared fields, expressions,
// operation contracts and the resource and publication graphs. It reads no
// environment values. Settings, inputs and metadata that hold expressions are
// checked by schema, and their values when a command uses them. Resolved
// validation evaluates everything a run would and returns the project with its
// connection settings evaluated.
func ValidateProject(ctx context.Context, opts Options, resolved bool) (result config.Project, err error) {
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return p, err
	}
	if err := Validate(ctx, p); err != nil {
		return p, err
	}
	done := plugin.Stage(ctx, "Validating project")
	defer func() { done(err) }()
	ops, cleanup, err := projectOperations(ctx, p, opts)
	if err != nil {
		return p, err
	}
	defer cleanup()
	// Validation answers whether the whole catalog is valid, so every declared
	// resource is a root, including the suspended resources that runs skip.
	plans, selected, err := discoverClosure(ctx, p, ops, sortedKeys(p.Resources), resolved, true)
	if err != nil {
		return p, err
	}
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return p, err
	}
	if err := verifyIcons(root, plans, selected); err != nil {
		return p, err
	}
	if _, err := planDestinations(ctx, p, plans, ops, root, selected, resolved); err != nil {
		return p, err
	}
	if !resolved {
		return p, nil
	}
	evaluated, err := p.Resolved()
	if err != nil {
		return p, err
	}
	used := map[string]bool{}
	for _, plan := range plans {
		for name := range plan.Destinations {
			used[name] = true
		}
	}
	for _, name := range sortedKeys(used) {
		destination := evaluated.Destinations[name]
		if err := ops.configuration(destination.Operation, destination.Config, true); err != nil {
			return p, fmt.Errorf("destination %s: %w", name, err)
		}
	}
	return evaluated, nil
}

func projectOperations(ctx context.Context, p config.Project, opts Options) (*operations, func(), error) {
	if len(p.Plugins) == 0 {
		ops, err := builtins(opts.Handlers)
		return ops, func() {}, err
	}
	store, err := cas.Open(opts.CacheDir)
	if err != nil {
		return nil, nil, err
	}
	release, err := store.Lease(ctx)
	if err != nil {
		return nil, nil, err
	}
	work, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "operations-*")
	if err != nil {
		_ = release()
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(work); _ = release() }
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	ops, err := loadOperations(ctx, p, source.New(store, root, opts.Lock.Offline), work, opts.Handlers, false)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return ops, cleanup, nil
}

// configuration checks connection settings against the operation's schema.
// Settings as written may hold expressions in place of values; resolved
// settings must satisfy the schema itself.
func (o *operations) configuration(name string, settings map[string]any, resolved bool) error {
	op, err := o.operation(name)
	if err != nil {
		return err
	}
	schema := op.ConfigSchema
	if !resolved {
		if schema, err = config.ExpressionSchema(schema); err != nil {
			return err
		}
	}
	if len(schema) == 0 {
		return nil
	}
	if settings == nil {
		settings = map[string]any{}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := plugin.ValidateSchema(schema, data); err != nil {
		return fmt.Errorf("operation %s config: %w", name, err)
	}
	return nil
}

// validateInput checks a non-resource input against its resolver. Settings
// that hold expressions are checked by the resolver's schema, which accepts
// them, and their values when a run evaluates them.
func (o *operations) validateInput(ctx context.Context, input plugin.Input) error {
	written := expression.Has(input.Config)
	var schema, data json.RawMessage
	var err error
	if source.NativeResolver(input.Resolver) {
		if !written {
			return source.ValidateInput(input)
		}
		if schema, err = nativeInputSchema(); err != nil {
			return err
		}
		data, err = json.Marshal(input)
	} else {
		operation, lookupErr := o.lookup(input.Resolver, "resolver")
		if lookupErr != nil {
			return lookupErr
		}
		if operation.Resolver == nil {
			return fmt.Errorf("unknown resolver %q", input.Resolver)
		}
		data, err = json.Marshal(input.Config)
		if err != nil {
			return err
		}
		if !written {
			return o.call(ctx, input.Resolver, "validate", plugin.ResolveRequest[json.RawMessage]{Config: data, Base: input.Base}, nil)
		}
		schema, err = config.ExpressionSchema(operation.ConfigSchema)
	}
	if err != nil || len(schema) == 0 {
		return err
	}
	return plugin.ValidateSchema(schema, data)
}

// nativeInputSchema describes the built-in resolvers' settings, accepting
// expressions in place of values.
var nativeInputSchema = sync.OnceValues(func() (json.RawMessage, error) {
	data, err := json.Marshal(source.InputSchema(reflect.TypeFor[plugin.Input]()))
	if err != nil {
		return nil, err
	}
	return config.ExpressionSchema(data)
})

type reconcileHandler func(context.Context, plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error)

type operations struct {
	registry *plugin.Registry
	identity map[string]string
	plugins  map[string]plugins.Entry
	// unavailable explains each operation a plugin offers at another interface
	// version, by name and by resource kind.
	unavailable      map[string]error
	unavailableKinds map[plugin.ResourceKind]error
	// failed explains each plugin that did not load.
	failed map[string]error
}

func schemaJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func builtins(handlers map[string]reconcileHandler) (*operations, error) {
	ops := &operations{registry: plugin.New("stemma", "operations/4"), identity: map[string]string{}, unavailable: map[string]error{}, unavailableKinds: map[plugin.ResourceKind]error{}, failed: map[string]error{}}
	for _, err := range []error{
		plugin.Register(ops.registry, plugin.Operation{
			Name: "munki", Kind: "reconcile", SideEffects: "remote", Methods: []string{"validate", "plan", "apply"},
			RequiresInspection: true, Content: &plugin.ContentContract{Formats: []string{"pkg", "dmg"}, SourceFree: true},
			MetadataSchema: schemaJSON(munkirepo.MetadataSchema()),
		}, munkirepo.Handle),
		plugin.Register(ops.registry, plugin.Operation{
			Name: "intune", Kind: "reconcile", SideEffects: "remote", Methods: []string{"validate", "plan", "apply"},
			RequiresInspection: true, Content: intune.ContentContract(), Requirements: intune.RuntimeRequirements(),
			MetadataSchema: schemaJSON(intune.MetadataSchema()),
		}, intune.Handle),
		plugin.Register(ops.registry, plugin.Operation{
			Name: "jamf", Kind: "reconcile", SideEffects: "remote", Methods: []string{"validate", "plan", "apply"},
			Content: &plugin.ContentContract{Formats: []string{"pkg", "dmg"}}, MetadataSchema: schemaJSON(jamf.MetadataSchema()),
		}, jamf.Handle),
	} {
		if err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"munki", "intune", "jamf"} {
		ops.identity[name] = "stemma/operations/4"
	}
	if err := registerKinds(ops); err != nil {
		return nil, err
	}
	if len(handlers) != 0 {
		registry := plugin.New("stemma", "operations/4")
		for _, operation := range ops.registry.Descriptor().Operations {
			handler := ops.registry.Handle
			if replacement := handlers[operation.Name]; replacement != nil {
				handler = func(ctx context.Context, request plugin.Request) (plugin.Response, error) {
					var input plugin.ReconcileRequest[json.RawMessage]
					if err := json.Unmarshal(request.Input, &input); err != nil {
						return plugin.Response{}, err
					}
					input.Method = request.Method
					output, err := replacement(ctx, input)
					data, encodeErr := json.Marshal(output)
					return plugin.Response{Output: data}, errors.Join(err, encodeErr)
				}
			}
			if err := registry.Register(operation, handler); err != nil {
				return nil, err
			}
		}
		ops.registry = registry
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	file, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	implementation := hex.EncodeToString(hash.Sum(nil))
	for name, version := range ops.identity {
		ops.identity[name] = version + "/" + implementation
	}
	return ops, nil
}

// operation finds a registered operation. A name a plugin offers at another
// interface version fails with that reason.
func (o *operations) operation(name string) (plugin.Operation, error) {
	return o.lookup(name, "operation")
}

func (o *operations) lookup(name, noun string) (plugin.Operation, error) {
	for _, operation := range o.registry.Descriptor().Operations {
		if operation.Name == name {
			return operation, nil
		}
	}
	if err, ok := o.unavailable[name]; ok {
		return plugin.Operation{}, err
	}
	return plugin.Operation{}, o.missing(fmt.Sprintf("unknown %s %q", noun, name))
}

func (o *operations) call(ctx context.Context, name, method string, input, output any) error {
	logger := plugin.Logger(ctx).With("operation", name, "method", method)
	started := time.Now()
	logger.DebugContext(ctx, "Invoking operation")
	defer func() {
		logger.DebugContext(ctx, "Operation returned", "elapsed", time.Since(started).Round(time.Millisecond))
	}()
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	response, callErr := o.registry.Handle(ctx, plugin.Request{Operation: name, Method: method, Input: data})
	if output != nil && len(response.Output) > 0 {
		if err := json.Unmarshal(response.Output, output); err != nil {
			return errors.Join(callErr, fmt.Errorf("operation %s output: %w", name, err))
		}
	}
	return callErr
}

// loadOperations registers the built-in operations and those each declared
// plugin offers. A plugin that does not load, or offers operations at another
// interface version, fails only the runs that use what it would provide. Only
// a run that resolves plugins changes their lock entries: it locks a tag or
// local path whose entry is missing or stale and keeps the entry of a plugin
// that does not load.
func loadOperations(ctx context.Context, p config.Project, manager *source.Manager, work string, handlers map[string]reconcileHandler, resolve bool) (*operations, error) {
	ops, err := builtins(handlers)
	if err != nil || len(p.Plugins) == 0 {
		return ops, err
	}
	locked, err := lockfile.Load(lockfile.Filename(manager.Root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ops.plugins = maps.Clone(locked.Plugins)
	if resolve {
		ops.plugins = map[string]plugins.Entry{}
	}
	store := plugins.New(manager.Store, manager.Offline)
	for _, name := range slices.Sorted(maps.Keys(p.Plugins)) {
		loaded := loadPlugin(ctx, store, manager.Root, work, name, p.Plugins[name], locked.Plugins[name], resolve)
		entry := loaded.entry
		if !ops.add(loaded) {
			entry = locked.Plugins[name]
			if resolve {
				plugin.Logger(ctx).WarnContext(ctx, "Plugin "+name+" did not load", "error", ops.failed[name])
			}
		}
		if resolve && entry != (plugins.Entry{}) {
			ops.plugins[name] = entry
		}
	}
	return ops, nil
}
