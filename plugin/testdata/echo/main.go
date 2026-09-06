package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/woodleighschool/stemma/plugin"
)

func main() {
	if plugin.Serve(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, request plugin.Request) (plugin.Response, error) {
		var config struct {
			WaitURL string `json:"wait_url"`
			Fail    bool   `json:"fail"`
		}
		if err := json.Unmarshal(request.Config, &config); len(request.Config) > 0 && err != nil {
			return plugin.Response{}, err
		}
		if config.WaitURL != "" {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.WaitURL, nil)
			if err != nil {
				return plugin.Response{}, err
			}
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				return plugin.Response{}, err
			}
			_ = response.Body.Close()
		}
		if request.Method == "plan" {
			return plugin.Response{Changes: []plugin.Change{
				{Kind: "metadata", Field: "config", Action: "replace", After: request.Config},
				{Kind: "metadata", Field: "metadata", Action: "replace", After: request.Metadata},
			}}, nil
		}
		if config.Fail {
			return plugin.Response{Binding: request.Binding}, errors.New("later upload failed")
		}
		return plugin.Response{Binding: request.Binding}, nil
	}) != nil {
		os.Exit(1)
	}
}
