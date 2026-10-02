package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

func registerResolvers(manager *source.Manager, ops *operations, work string) error {
	for _, operation := range ops.registry.Descriptor().Operations {
		if operation.Resolver == nil {
			continue
		}
		call := func(ctx context.Context, method string, input plugin.Input, observation json.RawMessage, workspace string) (plugin.ResolveResponse, error) {
			settings, err := json.Marshal(input.Config)
			if err != nil {
				return plugin.ResolveResponse{}, err
			}
			request := plugin.ResolveRequest[json.RawMessage]{Config: settings, Base: input.Base, Root: manager.Root, Workspace: workspace, Observation: observation}
			if err := ops.call(ctx, operation.Name, "validate", request, nil); err != nil {
				return plugin.ResolveResponse{}, err
			}
			var response plugin.ResolveResponse
			err = ops.call(ctx, operation.Name, method, request, &response)
			return response, err
		}
		resolver := source.Resolver{
			Version: operation.Resolver.Version, Identity: ops.identity[operation.Name], Local: operation.Resolver.Local,
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
			Discover: func(ctx context.Context, input plugin.Input) (source.Discovery, error) {
				response, err := call(ctx, "discover", input, nil, "")
				if err == nil && (response.Download != nil || response.Artifact.Path != "") {
					return source.Discovery{}, errors.New("resolver discovery must not return acquisition instructions")
				}
				result := source.Discovery{Observation: response.Observation, Immutable: response.Immutable, Evidence: response.Evidence, Version: response.Version, ContentRoot: response.ContentRoot}
				if response.Content != nil {
					result.Content = &source.Content{SHA256: response.Content.SHA256, Filename: response.Content.Filename, Mode: response.Content.Mode, Tree: response.Content.Tree}
				}
				return result, err
			},
			Acquire: func(ctx context.Context, input plugin.Input, observation json.RawMessage) (source.Acquisition, error) {
				workspace, err := os.MkdirTemp(work, "resolver-*")
				if err != nil {
					return source.Acquisition{}, err
				}
				// Manager imports these bytes before the enclosing operation lease ends.
				response, err := call(ctx, "run", input, observation, workspace)
				if err != nil {
					return source.Acquisition{}, err
				}
				if response.Download != nil {
					if response.Artifact.Path != "" {
						return source.Acquisition{}, errors.New("resolver run must return exactly one download or artifact")
					}
					return source.Acquisition{Download: &source.Download{Download: *response.Download}, Evidence: response.Evidence}, nil
				}
				if response.Artifact.Path == "" {
					return source.Acquisition{}, errors.New("resolver run must return exactly one download or artifact")
				}
				resolved, err := filepath.EvalSymlinks(response.Artifact.Path)
				if err != nil {
					return source.Acquisition{}, err
				}
				allowed, err := filepath.EvalSymlinks(workspace)
				if err != nil {
					return source.Acquisition{}, err
				}
				if !within(allowed, resolved) {
					return source.Acquisition{}, errors.New("resolver output must be inside its leased workspace")
				}
				response.Artifact.Path = resolved
				return source.Acquisition{Artifact: &response.Artifact, Evidence: response.Evidence}, nil
			},
		}
		if err := manager.Register(operation.Name, resolver); err != nil {
			return err
		}
	}
	return nil
}
