package source

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/woodleighschool/stemma/plugin"
)

func builtins(m *Manager) map[string]Resolver {
	return map[string]Resolver{
		"http":     m.httpResolver(),
		"github":   m.githubResolver(),
		"file":     m.fileResolver(),
		"local":    m.localResolver(),
		"homebrew": m.homebrewResolver(),
		"winget":   m.wingetResolver(),
	}
}

var builtinDescriptors = builtins(nil)

func resolverFor[C any](parse func(plugin.Input) (C, error), discover func(context.Context, C) (Discovery, error), acquire func(context.Context, C, json.RawMessage) (Acquisition, error)) Resolver {
	schema := plugin.SchemaFor[C]()
	schema.ID, schema.Version = "", ""
	for _, name := range schema.Required {
		property, ok := schema.Properties.Get(name)
		if !ok {
			continue
		}
		if property.Type == "string" {
			property.MinLength = new(uint64(1))
		}
		if property.Type == "array" {
			property.MinItems = new(uint64(1))
		}
	}
	return Resolver{
		Version: "2", Schema: schema,
		Fingerprint: func(input plugin.Input) (string, error) {
			config, err := parse(input)
			if err != nil {
				return "", err
			}
			var identity any = config
			if owned, ok := any(config).(interface{ sourceIdentity() any }); ok {
				identity = owned.sourceIdentity()
			}
			return fingerprint(identity)
		},
		Discover: func(ctx context.Context, input plugin.Input) (Discovery, error) {
			config, err := parse(input)
			if err != nil {
				return Discovery{}, err
			}
			return discover(ctx, config)
		},
		Acquire: func(ctx context.Context, input plugin.Input, observation json.RawMessage) (Acquisition, error) {
			config, err := parse(input)
			if err != nil {
				return Acquisition{}, err
			}
			return acquire(ctx, config, observation)
		},
	}
}

// Resolvers lists built-in resolver registrations.
func Resolvers() []string { return slices.Sorted(maps.Keys(builtinDescriptors)) }

// NativeResolver reports whether a built-in registration reserves the name.
func NativeResolver(name string) bool { _, ok := builtinDescriptors[name]; return ok }

// ValidateInput validates a built-in declaration without acquisition.
func ValidateInput(input plugin.Input) error {
	resolver, ok := builtinDescriptors[input.Resolver]
	if !ok {
		return fmt.Errorf("unsupported input resolver %q", input.Resolver)
	}
	_, err := resolver.Fingerprint(input)
	return err
}

// InputChanged asks the resolver about project changes. Local executable
// resolvers without declared dependencies conservatively depend on the project.
func (m *Manager) InputChanged(input plugin.Input, changed []string) (bool, error) {
	resolver, err := m.resolver(input.Resolver)
	if err != nil {
		return false, err
	}
	if resolver.Changed != nil {
		return resolver.Changed(input, changed)
	}
	return resolver.Local && len(changed) > 0, nil
}
