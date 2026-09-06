package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestApplyBookkeepingAndBindingPresence(t *testing.T) {
	failed := errors.New("upload failed after creating destination")
	for _, test := range []struct {
		name, previous, returned, wantBinding string
		err                                   error
	}{
		{name: "success without binding", wantBinding: `null`},
		{name: "omission preserves binding", previous: `{"id":1}`, wantBinding: `{"id":1}`},
		{name: "null clears binding", previous: `{"id":1}`, returned: `null`, wantBinding: `null`},
		{name: "value replaces binding", previous: `{"id":1}`, returned: `{"id":2}`, wantBinding: `{"id":2}`},
		{name: "first failure retains new binding", returned: `{"id":2}`, wantBinding: `{"id":2}`, err: failed},
		{name: "partial failure retains binding", previous: `{"id":1}`, returned: `{"id":2}`, wantBinding: `{"id":2}`, err: failed},
		{name: "failed omission preserves binding", previous: `{"id":1}`, wantBinding: `{"id":1}`, err: failed},
		{name: "failed null clears binding", previous: `{"id":1}`, returned: `null`, wantBinding: `null`, err: failed},
		{name: "cancellation preserves previous success", previous: `{"id":1}`, wantBinding: `{"id":1}`, err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "stemma.yaml")
			manifest := `version: 1
project: bookkeeping
recipes:
  app:
    source: {type: file, path: installer.bin}
    destinations:
      local: {}
destinations:
  local: {type: munki, path: repo}
`
			if err := os.WriteFile(configPath, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "installer.bin"), []byte("new installer bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			project, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			stateDir := t.TempDir()
			statePath := filepath.Join(stateDir, "bookkeeping.json")
			previous := binding{
				Connection: config.Fingerprint(project.Destinations["local"]),
				Binding:    json.RawMessage(test.previous),
			}
			if test.previous != "" {
				previous.Source = strings.Repeat("1", 64)
				previous.Payload = strings.Repeat("2", 64)
				if err := saveState(statePath, state{Version: 1, Project: project.Project, Bindings: map[string]binding{"app/local": previous}}); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			options := Options{
				ConfigPath: configPath, CacheDir: t.TempDir(), StateDir: stateDir, Method: "apply",
				Handlers: map[string]plugin.Handler{"munki": func(_ context.Context, request plugin.Request) (plugin.Response, error) {
					called = true
					if request.Method != "apply" || compactJSON(t, request.Binding) != test.previous {
						t.Fatalf("apply request: %+v", request)
					}
					return plugin.Response{Binding: json.RawMessage(test.returned)}, test.err
				}},
			}
			report, err := Run(t.Context(), options)
			if !errors.Is(err, test.err) || !called || len(report.Recipes) != 1 || len(report.Recipes[0].Destinations) != 1 {
				t.Fatalf("apply: report=%+v error=%v called=%v", report, err, called)
			}
			destination := report.Recipes[0].Destinations[0]
			if destination.Applied != (test.err == nil) || (destination.Error != "") != (test.err != nil) || !destination.SourceChanged || !destination.PreparedChanged {
				t.Fatalf("apply report: %+v", destination)
			}
			current, err := loadState(statePath, project.Project)
			if err != nil {
				t.Fatal(err)
			}
			got, exists := current.Bindings["app/local"]
			wantSource, wantPayload := previous.Source, previous.Payload
			if test.err == nil {
				wantSource = report.Recipes[0].Prepared.Source.Artifact.SHA256
				wantPayload = report.Recipes[0].Prepared.Payload.SHA256
			}
			if !exists || got.Connection != previous.Connection || got.Source != wantSource || got.Payload != wantPayload || compactJSON(t, got.Binding) != test.wantBinding {
				t.Fatalf("persisted state: %+v, want source=%s payload=%s binding=%s", got, wantSource, wantPayload, test.wantBinding)
			}
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			options.Method = "plan"
			options.Handlers["munki"] = func(_ context.Context, request plugin.Request) (plugin.Response, error) {
				if request.Method != "plan" || compactJSON(t, request.Binding) != test.wantBinding {
					t.Fatalf("plan did not receive persisted binding: %+v", request)
				}
				return plugin.Response{Binding: json.RawMessage(`{"id":3}`)}, nil
			}
			plan, err := Run(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			next := plan.Recipes[0].Destinations[0]
			if next.Applied || next.SourceChanged != (test.err != nil) || next.PreparedChanged != (test.err != nil) {
				t.Fatalf("next plan: %+v", next)
			}
			after, err := os.ReadFile(statePath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("planning changed durable state: %v", err)
			}
		})
	}
}

func compactJSON(t *testing.T, data json.RawMessage) string {
	t.Helper()
	if len(data) == 0 {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatal(err)
	}
	return compact.String()
}

func TestNativeValidationBeforeAcquisition(t *testing.T) {
	root := t.TempDir()
	project := config.Project{
		Project: "validation",
		Recipes: map[string]config.Recipe{"app": {
			Destinations: map[string]map[string]any{"local": {"unattended_install": false}},
		}},
		Destinations: map[string]config.Destination{"local": {Type: "munki", Path: filepath.Join(root, "repo")}},
	}
	if err := Validate(t.Context(), project); err != nil {
		t.Fatalf("native validation rejected supported metadata: %v", err)
	}
	if _, err := os.Stat(project.Destinations["local"].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native validation touched the destination: %v", err)
	}
	manifest := `version: 1
project: validation
recipes:
  app:
    source: {type: file, path: missing.pkg}
    destinations:
      local: {unattended_install: invalid}
destinations:
  local: {type: munki, path: repo}
`
	configPath := filepath.Join(root, "stemma.yaml")
	if err := os.WriteFile(configPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(t.Context(), Options{ConfigPath: configPath, CacheDir: filepath.Join(root, "cache"), Method: "apply"})
	if err == nil || !strings.Contains(err.Error(), "unattended_install") {
		t.Fatalf("expected validation before acquiring the missing installer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid metadata reached acquisition: %v", err)
	}
	project.Destinations["local"] = config.Destination{Type: "plugin", Plugin: "external", Config: map[string]any{"opaque": nil}}
	project.Recipes["app"].Destinations["local"] = map[string]any{"plugin_owned": false}
	if err := Validate(t.Context(), project); err != nil {
		t.Fatalf("native validation interpreted plugin-owned input: %v", err)
	}
}
