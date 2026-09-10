package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/woodleighschool/stemma/plugin"
)

func main() {
	if mode := os.Getenv("STEMMA_ECHO_RESPONSE"); mode != "" {
		fmt.Fprint(os.Stdout, mode)
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
	if err := registry.Register(plugin.Operation{
		Name: "echo.inspect", Kind: "inspect", SideEffects: "none", Methods: []string{"validate", "run"},
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"config":true,"inputs":{"type":"object"},"workspace":{"type":"string"},"timestamp":{"type":"string"}},"required":["workspace"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"artifacts":{"type":"object"},"facts":{"type":"object"}},"required":["facts"],"additionalProperties":false}`),
	}, inspect); err != nil {
		os.Exit(1)
	}
	if plugin.Serve(context.Background(), os.Stdin, os.Stdout, registry) != nil {
		os.Exit(1)
	}
}

func reconcile(ctx context.Context, envelope plugin.Request) (plugin.Response, error) {
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

func inspect(_ context.Context, envelope plugin.Request) (plugin.Response, error) {
	if envelope.Method == "validate" {
		return plugin.Response{}, nil
	}
	var request plugin.StepRequest
	if err := json.Unmarshal(envelope.Input, &request); err != nil {
		return plugin.Response{}, err
	}
	output, err := json.Marshal(plugin.StepResponse{Artifacts: request.Inputs, Facts: plugin.Facts{Version: plugin.FactsVersion}})
	return plugin.Response{Output: output}, err
}
