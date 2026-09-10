package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/intune"
	"github.com/woodleighschool/stemma/internal/jamf"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/internal/munkirepo"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Catalog describes built-in and installed plugin operations without acquiring
// software inputs or contacting destinations. Trusted plugin discovery executes code.
func Catalog(ctx context.Context, opts Options) (plugin.Descriptor, error) {
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return plugin.Descriptor{}, err
	}
	ops, cleanup, err := projectOperations(ctx, p, opts)
	if err != nil {
		return plugin.Descriptor{}, err
	}
	defer cleanup()
	return ops.registry.Descriptor(), nil
}

// ValidateProject checks authored fields and available operation contracts.
// Values that depend on artifacts are validated after preparation.
func ValidateProject(ctx context.Context, opts Options) (config.Project, error) {
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return p, err
	}
	if err := Validate(ctx, p); err != nil {
		return p, err
	}
	ops, cleanup, err := projectOperations(ctx, p, opts)
	if err != nil {
		return p, err
	}
	defer cleanup()
	return p, checkOperations(p, ops)
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
	ops, err := loadOperations(ctx, p, source.New(store, root, opts.Lock.Offline), work, opts.Handlers)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return ops, cleanup, nil
}

func checkOperations(p config.Project, ops *operations) error {
	for name, software := range p.Software {
		for _, step := range software.Steps {
			if err := ops.check(step.Operation, true); err != nil {
				return fmt.Errorf("software %s step %s: %w", name, step.Name, err)
			}
			if err := ops.configuration(step.Operation, step.Config); err != nil {
				return fmt.Errorf("software %s step %s: %w", name, step.Name, err)
			}
		}
		for destination := range software.Destinations {
			if err := ops.check(p.Destinations[destination].Operation, false); err != nil {
				return fmt.Errorf("software %s destination %s: %w", name, destination, err)
			}
			settings := p.Destinations[destination].Config
			if p.Destinations[destination].Operation == "munki" {
				settings = map[string]any{"path": p.Destinations[destination].Path}
			}
			if err := ops.configuration(p.Destinations[destination].Operation, settings); err != nil {
				return fmt.Errorf("destination %s: %w", destination, err)
			}
		}
	}
	return nil
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
	if name == "pkg" {
		var spec config.Artifact
		if err := json.Unmarshal(data, &spec); err != nil {
			return err
		}
		if spec.Type == "" {
			spec.Type = "pkg"
		}
		return spec.Validate()
	}
	return nil
}

type reconcileHandler func(context.Context, plugin.ReconcileRequest) (plugin.ReconcileResponse, error)

type operations struct {
	registry *plugin.Registry
	identity map[string]string
}

func operationSchema(value any) json.RawMessage {
	r := jsonschema.Reflector{DoNotReference: true}
	data, _ := json.Marshal(r.Reflect(value))
	return data
}

func builtins(handlers map[string]reconcileHandler) (*operations, error) {
	ops := &operations{registry: plugin.New("stemma", "operations/2"), identity: map[string]string{}}
	register := func(name, kind, effects string, methods []string, input, output any, handler plugin.Handler) error {
		operation := plugin.Operation{Name: name, Kind: kind, SideEffects: effects, Methods: methods, InputSchema: operationSchema(input), OutputSchema: operationSchema(output)}
		switch name {
		case "pkg":
			schema := jsonschema.Reflector{DoNotReference: true}
			value := schema.Reflect(config.Artifact{})
			value.Required = slices.DeleteFunc(value.Required, func(field string) bool { return field == "type" })
			operation.ConfigSchema, _ = json.Marshal(value)
		case "inspect":
			operation.ConfigSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
		case "munki.pkginfo":
			metadata := munki.MetadataSchema()
			metadata.Required = []string{"name"}
			operation.ConfigSchema, _ = json.Marshal(metadata)
		case "munki":
			operation.ConfigSchema = json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string","minLength":1}},"additionalProperties":false}`)
		case "intune":
			operation.ConfigSchema, _ = json.Marshal(intune.ConnectionSchema())
		case "jamf":
			operation.ConfigSchema, _ = json.Marshal(jamf.ConnectionSchema())
		}
		if err := ops.registry.Register(operation, handler); err != nil {
			return err
		}
		ops.identity[name] = "stemma/operations/2"
		return nil
	}
	for _, name := range []string{"munki", "intune", "jamf"} {
		handler := nativeHandler(name)
		if handlers[name] != nil {
			handler = handlers[name]
		}
		if err := register(name, "reconcile", "remote", []string{"validate", "plan", "apply"}, plugin.ReconcileRequest{}, plugin.ReconcileResponse{}, func(ctx context.Context, request plugin.Request) (plugin.Response, error) {
			var input plugin.ReconcileRequest
			if err := json.Unmarshal(request.Input, &input); err != nil {
				return plugin.Response{}, err
			}
			input.Method = request.Method
			output, err := handler(ctx, input)
			data, encodeErr := json.Marshal(output)
			return plugin.Response{Output: data}, errors.Join(err, encodeErr)
		}); err != nil {
			return nil, err
		}
	}
	if err := register("inspect", "inspect", "none", []string{"validate", "run"}, plugin.StepRequest{}, plugin.StepResponse{}, inspectOperation); err != nil {
		return nil, err
	}
	if err := register("pkg", "package", "workspace", []string{"validate", "run"}, plugin.StepRequest{}, plugin.StepResponse{}, packageOperation); err != nil {
		return nil, err
	}
	if err := register("munki.pkginfo", "render", "workspace", []string{"validate", "run"}, plugin.StepRequest{}, plugin.StepResponse{}, munkiOperation); err != nil {
		return nil, err
	}
	ops.identity["pkg"] += "/" + pkgbuild.Version
	ops.identity["munki.pkginfo"] += "/4"
	return ops, nil
}

func munkiOperation(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	if err := ctx.Err(); err != nil {
		return plugin.Response{}, err
	}
	var input plugin.StepRequest
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return plugin.Response{}, err
	}
	artifact, exists := input.Inputs["input"]
	var authored map[string]any
	if err := json.Unmarshal(input.Config, &authored); err != nil {
		return plugin.Response{}, err
	}
	if authored["installer_type"] == "nopkg" {
		if len(input.Inputs) != 0 {
			return plugin.Response{}, errors.New("munki.pkginfo nopkg requires no installer inputs")
		}
	} else if !exists || len(input.Inputs) != 1 || artifact.Tree || artifact.Path == "" {
		return plugin.Response{}, errors.New("munki.pkginfo requires one file named input")
	}
	_, installs := authored["installs"]
	_, receipts := authored["receipts"]
	_, script := authored["installcheck_script"]
	if exists && !installs && !receipts && !script && !slices.ContainsFunc(artifact.Facts.Subjects, func(subject plugin.Subject) bool { return subject.App != nil }) {
		facts, err := inspection.Read(ctx, artifact.Path)
		if err != nil {
			return plugin.Response{}, fmt.Errorf("munki.pkginfo inspect installer: %w", err)
		}
		for _, subject := range facts.Subjects {
			if subject.App != nil {
				artifact.Facts.Subjects = append(artifact.Facts.Subjects, subject)
			}
		}
	}
	effective, _, err := resolveMetadata(config.Software{}, authored, artifact.Facts, "munki")
	if err != nil {
		return plugin.Response{}, err
	}
	metadata, err := json.Marshal(effective)
	if err != nil {
		return plugin.Response{}, err
	}
	base := munki.Input{}
	if exists {
		base = munki.Input{Version: artifact.Version, SHA256: artifact.SHA256, Size: artifact.Size, InstallerLocation: "stemma/" + artifact.SHA256 + "/" + artifact.Filename}
	}
	if artifact.Format == "pkg" || filepath.Ext(artifact.Filename) == ".pkg" {
		base.InstallerType = "pkg"
	}
	value, _, err := munki.Compose(base, metadata)
	if err != nil {
		return plugin.Response{}, err
	}
	document, err := munki.Render(value)
	if err != nil {
		return plugin.Response{}, err
	}
	if request.Method == "validate" {
		return plugin.Response{}, nil
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return plugin.Response{}, err
	}
	filename := filepath.Join(input.Workspace, "pkginfo.json")
	if err := os.WriteFile(filename, append(data, '\n'), 0o600); err != nil {
		return plugin.Response{}, err
	}
	output, err := json.Marshal(plugin.StepResponse{Artifacts: map[string]plugin.Artifact{"artifact": {Path: filename, Filename: "pkginfo.json"}}})
	return plugin.Response{Output: output}, err
}

func nativeHandler(name string) reconcileHandler {
	switch name {
	case "munki":
		return munkirepo.Handle
	case "intune":
		return intune.Handle
	case "jamf":
		return jamf.Handle
	default:
		return nil
	}
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

func loadOperations(ctx context.Context, p config.Project, manager *source.Manager, work string, handlers map[string]reconcileHandler) (*operations, error) {
	ops, err := builtins(handlers)
	if err != nil || len(p.Plugins) == 0 {
		return ops, err
	}
	locked, err := lockfile.Load(filepath.Join(manager.Root, "stemma.lock.yaml"))
	if err != nil {
		return nil, fmt.Errorf("plugin operations require installed locks; run stemma plugins install: %w", err)
	}
	names := make([]string, 0, len(p.Plugins))
	for name := range p.Plugins {
		names = append(names, name)
	}
	slices.Sort(names)
	pluginStore := plugins.New(manager.Store, manager.Offline)
	for _, name := range names {
		provider := p.Plugins[name]
		entry, exists := locked.Plugins[name]
		if !exists {
			return nil, fmt.Errorf("plugin %s is not locked; run stemma plugins install", name)
		}
		bundle, err := pluginStore.Acquire(ctx, provider.Image, entry)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", name, err)
		}
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
	}
	return ops, nil
}

func inspectOperation(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	var input plugin.StepRequest
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return plugin.Response{}, err
	}
	if len(input.Inputs) != 1 || input.Inputs["input"].Path == "" {
		return plugin.Response{}, errors.New("inspect requires one artifact named input")
	}
	if len(input.Config) > 0 && string(input.Config) != "{}" && string(input.Config) != "null" {
		return plugin.Response{}, errors.New("inspect has no configuration fields")
	}
	if request.Method == "validate" {
		return plugin.Response{}, nil
	}
	artifact := input.Inputs["input"]
	facts, err := inspection.Read(ctx, artifact.Path)
	if err != nil {
		return plugin.Response{}, err
	}
	artifact.Facts = facts
	data, err := json.Marshal(plugin.StepResponse{Artifacts: map[string]plugin.Artifact{"artifact": artifact}, Facts: facts})
	return plugin.Response{Output: data}, err
}

func packageOperation(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	var input plugin.StepRequest
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return plugin.Response{}, err
	}
	var spec config.Artifact
	decoder := json.NewDecoder(bytes.NewReader(input.Config))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return plugin.Response{}, err
	}
	if spec.Type == "" {
		spec.Type = "pkg"
	}
	if err := spec.Validate(); err != nil {
		return plugin.Response{}, err
	}
	artifact, exists := input.Inputs["input"]
	if len(input.Inputs) != 1 || !exists || !artifact.Tree {
		return plugin.Response{}, errors.New("pkg requires one directory artifact named input")
	}
	if input.Timestamp.IsZero() {
		return plugin.Response{}, errors.New("pkg requires a stable input timestamp")
	}
	if request.Method == "validate" {
		return plugin.Response{}, nil
	}
	filename := spec.Filename
	if filename == "" {
		filename = spec.Identifier + ".pkg"
	}
	output := filepath.Join(input.Workspace, filename)
	if err := pkgbuild.Build(ctx, artifact.Path, output, pkgbuild.Options{Identifier: spec.Identifier, Version: spec.Version, Payload: spec.Payload, InstallLocation: spec.InstallLocation, Scripts: spec.Scripts, Timestamp: input.Timestamp}); err != nil {
		return plugin.Response{}, err
	}
	data, err := json.Marshal(plugin.StepResponse{Artifacts: map[string]plugin.Artifact{"artifact": {Path: output, Filename: filename, Format: "pkg"}}})
	return plugin.Response{Output: data}, err
}
