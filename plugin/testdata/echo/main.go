package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/plugin"
)

func main() {
	if mode := os.Getenv("STEMMA_ECHO_RESPONSE"); mode != "" {
		fmt.Fprintln(os.Stdout, mode)
		fmt.Fprint(os.Stderr, "synthetic diagnostic credential")
		if url := os.Getenv("STEMMA_ECHO_WAIT_URL"); url != "" {
			if err := waitForResponse(context.Background(), url); err != nil {
				os.Exit(1)
			}
		}
		if os.Getenv("STEMMA_ECHO_FAIL") != "" {
			os.Exit(1)
		}
		return
	}
	registry := plugin.New("echo", "1.0.0")
	if err := registry.Register(plugin.Operation{
		Name: "echo.reconcile", Kind: "reconcile", SideEffects: "remote", Methods: []string{"validate", "plan", "apply"},
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"fail":{"type":"boolean"},"wait_url":{"type":"string"}}}`),
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"identity":{"type":"object"},"config":true,"metadata":true,"peers":{"type":"object"},"subjects":{"type":"object"},"prepared":{"type":"boolean"},"root":{"type":"string"},"artifact":{"type":"object"},"inputs":{"type":"object","additionalProperties":{"type":"object"}},"facts":{"type":"object"}},"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"changes":{"type":"array"}},"additionalProperties":false}`),
	}, reconcile); err != nil {
		os.Exit(1)
	}

	var requirements []plugin.Requirement
	if tool := os.Getenv("STEMMA_ECHO_REQUIRE_TOOL"); tool != "" {
		requirements = []plugin.Requirement{{Command: tool, Purpose: "fixture build", Setup: "install the fixture helper"}}
	}
	if err := plugin.Register(registry, plugin.Operation{
		Name: "echo.build", Requirements: requirements, Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "example.test/v1", Kind: "ExternalInstaller"}, SideEffects: "workspace", Methods: []string{"validate", "run"},
	}, build); err != nil {
		panic(err)
	}
	if err := plugin.Register(registry, plugin.Operation{
		Name: "echo.download", Kind: "resolve", Resolver: &plugin.ResolverKind{Version: "1"}, SideEffects: "workspace", Methods: []string{"validate", "run"},
	}, download); err != nil {
		panic(err)
	}
	if plugin.Serve(context.Background(), os.Stdin, os.Stdout, registry) != nil {
		os.Exit(1)
	}
}

func reconcile(ctx context.Context, envelope plugin.Request) (plugin.Response, error) {
	plugin.Logger(ctx).DebugContext(ctx, "Fixture request")
	var request plugin.ReconcileRequest[json.RawMessage]
	if err := json.Unmarshal(envelope.Input, &request); err != nil {
		return plugin.Response{}, err
	}
	var config struct {
		WaitURL string `json:"wait_url"`
		Fail    bool   `json:"fail"`
	}
	if err := json.Unmarshal(request.Config, &config); len(request.Config) > 0 && err != nil {
		return plugin.Response{}, err
	}
	if config.WaitURL != "" {
		done := plugin.Stage(ctx, "Waiting for fixture")
		defer func() { done(ctx.Err()) }()
		plugin.Logger(ctx).InfoContext(ctx, "Transfer progress", "progress", true, "current", 3<<20, "total", 7<<20, "unit", "bytes")
		if err := waitForResponse(ctx, config.WaitURL); err != nil {
			return plugin.Response{}, err
		}
	}
	if envelope.Method == "validate" {
		return plugin.Response{}, nil
	}
	result := plugin.ReconcileResponse{Changes: []plugin.Change{{Kind: "metadata", Field: "applied", Action: "replace"}}}
	if envelope.Method == "plan" {
		result.Changes = []plugin.Change{
			{Kind: "metadata", Field: "config", Action: "replace", After: request.Config},
			{Kind: "metadata", Field: "metadata", Action: "replace", After: request.Metadata},
		}
	}
	output, err := json.Marshal(result)
	if config.Fail {
		err = errors.New("later upload failed")
	}
	return plugin.Response{Output: output}, err
}

func waitForResponse(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

type buildConfig struct {
	Source plugin.Input `json:"source" jsonschema_description:"Vendor installer input."`
}

func build(_ context.Context, request plugin.ResourceRequest[buildConfig]) (plugin.ResourceResult, error) {
	if request.Method == "validate" {
		return plugin.ResourceResult{Inputs: map[string]plugin.Input{"vendor": request.Config.Source}, Config: json.RawMessage(`{}`)}, nil
	}
	artifact := request.Inputs["vendor"]
	artifact.Format = "pkg"
	artifact.Evidence = map[string]json.RawMessage{"vendor.probe": json.RawMessage(`{"revision":7,"enabled":false}`)}
	return plugin.ResourceResult{Artifacts: map[string]plugin.Artifact{"installer": artifact}}, nil
}

type downloadConfig struct {
	URL string `json:"url" jsonschema_description:"Fixture download URL."`
}

func download(ctx context.Context, request plugin.ResolveRequest[downloadConfig]) (plugin.ResolveResponse, error) {
	config := struct {
		URL      string `json:"url"`
		Revision string `json:"revision,omitempty"`
	}{URL: request.Config.URL}
	if request.Locked {
		if err := json.Unmarshal(request.Observation, &config); err != nil {
			return plugin.ResolveResponse{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.URL, nil)
	if err != nil {
		return plugin.ResolveResponse{}, err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return plugin.ResolveResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return plugin.ResolveResponse{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	filename := filepath.Join(request.Workspace, "vendor.pkg")
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return plugin.ResolveResponse{}, err
	}
	_, err = io.Copy(file, response.Body)
	err = errors.Join(err, file.Close())
	if err != nil {
		return plugin.ResolveResponse{}, err
	}
	config.Revision = response.Header.Get("X-Fixture-Revision")
	observation, err := json.Marshal(config)
	if err != nil {
		return plugin.ResolveResponse{}, err
	}
	artifact := plugin.Artifact{Path: filename, Filename: "vendor.pkg"}
	if version := response.Header.Get("X-Fixture-Version"); version != "" {
		evidence, err := json.Marshal(map[string]string{"version": version})
		if err != nil {
			return plugin.ResolveResponse{}, err
		}
		artifact.Evidence = map[string]json.RawMessage{"vendor.release": evidence}
	}
	return plugin.ResolveResponse{Observation: observation, Artifact: artifact}, nil
}
