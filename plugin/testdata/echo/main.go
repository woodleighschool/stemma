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
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"method":{"type":"string"},"identity":{"type":"object"},"config":true,"metadata":true,"binding":true,"bindings":{"type":"object"},"subjects":{"type":"object"},"prepared":{"type":"boolean"},"root":{"type":"string"},"artifact":{"type":"object"},"inputs":{"type":"object","additionalProperties":{"type":"object"}},"facts":{"type":"object"}},"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"changes":{"type":"array"},"binding":true},"additionalProperties":false}`),
	}, reconcile); err != nil {
		os.Exit(1)
	}

	var requirements []plugin.Requirement
	if tool := os.Getenv("STEMMA_ECHO_REQUIRE_TOOL"); tool != "" {
		requirements = []plugin.Requirement{{Command: tool, Purpose: "fixture build", Setup: "install the fixture helper"}}
	}
	if err := registry.Register(plugin.Operation{
		Name: "echo.build", Requirements: requirements, Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "example.test/v1", Kind: "ExternalInstaller"}, SideEffects: "workspace", Methods: []string{"validate", "run"},
		ConfigSchema: json.RawMessage(`{"type":"object","required":["source"],"additionalProperties":false,"properties":{"source":{"type":"object"}}}`), InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
	}, build); err != nil {
		panic(err)
	}
	if err := registry.Register(plugin.Operation{
		Name: "echo.download", Kind: "resolve", Resolver: &plugin.ResolverKind{Version: "1"}, SideEffects: "workspace", Methods: []string{"validate", "run"},
		ConfigSchema: json.RawMessage(`{"type":"object","required":["url"],"additionalProperties":false,"properties":{"url":{"type":"string"}}}`), InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
	}, download); err != nil {
		panic(err)
	}
	if plugin.Serve(context.Background(), os.Stdin, os.Stdout, registry) != nil {
		os.Exit(1)
	}
}

func reconcile(ctx context.Context, envelope plugin.Request) (plugin.Response, error) {
	plugin.Logger(ctx).DebugContext(ctx, "Fixture request")
	var request plugin.ReconcileRequest
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
		plugin.Stage(ctx, "Waiting for fixture")
		if err := waitForResponse(ctx, config.WaitURL); err != nil {
			return plugin.Response{}, err
		}
	}
	if envelope.Method == "validate" {
		return plugin.Response{}, nil
	}
	result := plugin.ReconcileResponse{Binding: request.Binding}
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

func build(_ context.Context, envelope plugin.Request) (plugin.Response, error) {
	var request plugin.ResourceRequest
	if err := json.Unmarshal(envelope.Input, &request); err != nil {
		return plugin.Response{}, err
	}
	var result plugin.ResourceResult
	if envelope.Method == "validate" {
		var config struct {
			Source plugin.Input `json:"source"`
		}
		if err := json.Unmarshal(request.Config, &config); err != nil {
			return plugin.Response{}, err
		}
		result = plugin.ResourceResult{Inputs: map[string]plugin.Input{"vendor": config.Source}, Config: json.RawMessage(`{}`)}
	} else {
		artifact := request.Inputs["vendor"]
		artifact.Format = "pkg"
		artifact.Evidence = map[string]json.RawMessage{"vendor.probe": json.RawMessage(`{"revision":7,"enabled":false}`)}
		result.Artifacts = map[string]plugin.Artifact{"installer": artifact}
	}
	output, err := json.Marshal(result)
	return plugin.Response{Output: output}, err
}
func download(ctx context.Context, envelope plugin.Request) (plugin.Response, error) {
	var request plugin.ResolveRequest
	if err := json.Unmarshal(envelope.Input, &request); err != nil {
		return plugin.Response{}, err
	}
	if envelope.Method == "validate" {
		return plugin.Response{}, nil
	}
	var config struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(request.Config, &config); err != nil {
		return plugin.Response{}, err
	}
	if request.Locked {
		if err := json.Unmarshal(request.Observation, &config); err != nil {
			return plugin.Response{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.URL, nil)
	if err != nil {
		return plugin.Response{}, err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return plugin.Response{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return plugin.Response{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	filename := filepath.Join(request.Workspace, "vendor.pkg")
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return plugin.Response{}, err
	}
	_, err = io.Copy(file, response.Body)
	err = errors.Join(err, file.Close())
	if err != nil {
		return plugin.Response{}, err
	}
	observation, err := json.Marshal(config)
	if err != nil {
		return plugin.Response{}, err
	}
	output, err := json.Marshal(plugin.ResolveResponse{Observation: observation, Artifact: plugin.Artifact{Path: filename, Filename: "vendor.pkg"}})
	return plugin.Response{Output: output}, err
}
