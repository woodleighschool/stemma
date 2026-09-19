package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
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
	return ops.registry.Descriptor(), nil
}

// ValidateProject checks declared fields and available operation contracts.
// Values that depend on artifacts are validated after preparation.
func ValidateProject(ctx context.Context, opts Options) (result config.Project, err error) {
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
	plans, err := discover(ctx, p, ops)
	if err != nil {
		return p, err
	}
	selected, err := orderResources(plans, nil)
	if err != nil {
		return p, err
	}
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return p, err
	}
	if err := verifyIcons(root, plans, sortedKeys(plans)); err != nil {
		return p, err
	}
	_, err = planDestinations(ctx, p, plans, ops, root, selected)
	return p, err
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

func (o *operations) configuration(name string, settings map[string]any) error {
	op, err := o.operation(name)
	if err != nil {
		return err
	}
	if hasFactReference(settings) {
		return nil
	}
	if len(op.ConfigSchema) == 0 {
		return nil
	}
	if settings == nil {
		settings = map[string]any{}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := plugin.ValidateSchema(op.ConfigSchema, data); err != nil {
		return fmt.Errorf("operation %s config: %w", name, err)
	}

	return nil
}

type reconcileHandler func(context.Context, plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error)

type operations struct {
	registry *plugin.Registry
	identity map[string]string
	plugins  map[string]plugins.Entry
}

func schemaJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func builtins(handlers map[string]reconcileHandler) (*operations, error) {
	ops := &operations{registry: plugin.New("stemma", "operations/4"), identity: map[string]string{}}
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

func (o *operations) operation(name string) (plugin.Operation, error) {
	for _, operation := range o.registry.Descriptor().Operations {
		if operation.Name == name {
			return operation, nil
		}
	}
	return plugin.Operation{}, fmt.Errorf("unknown operation %q", name)
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
	response, callErr := o.registry.Handle(ctx, plugin.Request{Protocol: plugin.ProtocolVersion, Operation: name, Method: method, Input: data})
	if output != nil && len(response.Output) > 0 {
		if err := json.Unmarshal(response.Output, output); err != nil {
			return errors.Join(callErr, fmt.Errorf("operation %s output: %w", name, err))
		}
	}
	return callErr
}

func loadOperations(ctx context.Context, p config.Project, manager *source.Manager, work string, handlers map[string]reconcileHandler, frozen bool) (result *operations, runErr error) {
	ops, err := builtins(handlers)
	if err != nil || len(p.Plugins) == 0 {
		return ops, err
	}
	locked, err := lockfile.Load(lockfile.Filename(manager.Root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ops.plugins = map[string]plugins.Entry{}
	names := make([]string, 0, len(p.Plugins))
	for name := range p.Plugins {
		names = append(names, name)
	}
	slices.Sort(names)
	pluginStore := plugins.New(manager.Store, manager.Offline)
	for _, name := range names {
		ctx := plugin.WithLogger(ctx, plugin.Logger(ctx).With("plugin", name))
		done := plugin.Stage(ctx, "Loading plugin")
		defer func() { done(runErr) }()
		provider := p.Plugins[name]
		bundle, entry, err := pluginStore.Load(ctx, manager.Root, provider, locked.Plugins[name], frozen)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", name, err)
		}
		ops.plugins[name] = entry
		executable, err := pluginStore.Materialize(ctx, bundle, filepath.Join(work, name))
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", name, err)
		}
		response, err := plugin.Run(ctx, executable, plugin.Request{Method: "describe"})
		if err != nil {
			return nil, fmt.Errorf("plugin %s describe: %w", name, err)
		}
		var descriptor plugin.Descriptor
		if err := json.Unmarshal(response.Output, &descriptor); err != nil {
			return nil, fmt.Errorf("plugin %s descriptor: %w", name, err)
		}
		if err := plugin.ValidateDescriptor(descriptor); err != nil {
			return nil, fmt.Errorf("plugin %s descriptor: %w", name, err)
		}
		for _, operation := range descriptor.Operations {
			if operation.Resolver != nil && source.NativeResolver(operation.Name) {
				return nil, fmt.Errorf("plugin %s: resolver %q is built in", name, operation.Name)
			}
			if err := ops.registry.Register(operation, func(ctx context.Context, request plugin.Request) (plugin.Response, error) {
				return plugin.Run(ctx, executable, request)
			}); err != nil {
				return nil, fmt.Errorf("plugin %s: %w", name, err)
			}
			ops.identity[operation.Name] = config.Fingerprint(struct {
				Manifest   string
				Descriptor plugin.Descriptor
			}{bundle.Manifest, descriptor})
		}
		done(nil)
	}
	return ops, nil
}
