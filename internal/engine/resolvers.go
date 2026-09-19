package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

func registerResolvers(manager *source.Manager, ops *operations, work string) error {
	if manager.Resolvers == nil {
		manager.Resolvers = map[string]source.Resolver{}
	}
	for _, operation := range ops.registry.Descriptor().Operations {
		if operation.Resolver == nil {
			continue
		}
		if _, exists := manager.Resolvers[operation.Name]; exists {
			return fmt.Errorf("resolver %s is already registered", operation.Name)
		}
		resolve := func(ctx context.Context, input plugin.Input, observation json.RawMessage, locked bool) (source.Resolution, error) {
			workspace, err := os.MkdirTemp(work, "resolver-*")
			if err != nil {
				return source.Resolution{}, err
			}
			// Manager imports these bytes before the enclosing operation lease ends.
			settings, err := json.Marshal(input.Config)
			if err != nil {
				return source.Resolution{}, err
			}
			request := plugin.ResolveRequest[json.RawMessage]{Config: settings, Base: input.Base, Root: manager.Root, Workspace: workspace, Locked: locked, Observation: observation}
			if err := ops.call(ctx, operation.Name, "validate", request, nil); err != nil {
				return source.Resolution{}, err
			}
			var response plugin.ResolveResponse
			if err := ops.call(ctx, operation.Name, "run", request, &response); err != nil {
				return source.Resolution{}, err
			}
			resolved, err := filepath.EvalSymlinks(response.Artifact.Path)
			if err != nil {
				return source.Resolution{}, err
			}
			allowed, err := filepath.EvalSymlinks(workspace)
			if err != nil {
				return source.Resolution{}, err
			}
			if !within(allowed, resolved) {
				return source.Resolution{}, errors.New("resolver output must be inside its leased workspace")
			}
			response.Artifact.Path = resolved
			return source.Resolution{Observation: response.Observation, Artifact: response.Artifact}, nil
		}
		resolver := source.Resolver{
			Version: config.Fingerprint(struct{ Version, Implementation string }{operation.Resolver.Version, ops.identity[operation.Name]}), Local: operation.Resolver.Local,
			Fingerprint: func(input plugin.Input) (string, error) {
				var schema map[string]any
				if len(operation.ConfigSchema) > 0 {
					if err := json.Unmarshal(operation.ConfigSchema, &schema); err != nil {
						return "", err
					}
				}
				declaration, _ := connectionIdentity(input.Config, []map[string]any{schema}, schema)
				base := ""
				if operation.Resolver.Local {
					base = input.Base
				}
				return config.Fingerprint(struct {
					Declaration any
					Base        string
				}{declaration, base}), nil
			},
			Resolve: func(ctx context.Context, input plugin.Input) (source.Resolution, error) {
				return resolve(ctx, input, nil, false)
			},
			FetchLocked: func(ctx context.Context, input plugin.Input, observation json.RawMessage) (plugin.Artifact, error) {
				result, err := resolve(ctx, input, observation, true)
				return result.Artifact, err
			},
		}
		if err := manager.Register(operation.Name, resolver); err != nil {
			return err
		}
	}
	return nil
}
