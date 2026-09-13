package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestAuxiliaryCacheVariantsReuseInstaller(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ops := &operations{registry: plugin.New("fixture", "1"), identity: map[string]string{"fixture": "1"}}
	variant := "portable/1"
	builds, renders := 0, 0
	operation := plugin.Operation{Name: "fixture", Kind: "resource", SideEffects: "workspace", Methods: []string{"run"}, InputSchema: operationSchema(plugin.ResourceRequest{}), OutputSchema: operationSchema(plugin.ResourceResult{})}
	err = ops.registry.Register(operation, func(_ context.Context, request plugin.Request) (plugin.Response, error) {
		var input plugin.ResourceRequest
		if err := json.Unmarshal(request.Input, &input); err != nil {
			return plugin.Response{}, err
		}
		installer := input.Cached["installer"]
		if installer.Path == "" {
			builds++
			installer = plugin.Artifact{Path: filepath.Join(input.Workspace, "installer.pkg"), Filename: "installer.pkg", Format: "pkg", Version: "1"}
			if err := os.WriteFile(installer.Path, []byte("immutable installer"), 0o600); err != nil {
				return plugin.Response{}, err
			}
		}
		renders++
		icon := plugin.Artifact{Path: filepath.Join(input.Workspace, "icon.png"), Filename: "icon.png", Format: "png"}
		if err := os.WriteFile(icon.Path, []byte(variant), 0o600); err != nil {
			return plugin.Response{}, err
		}
		return resourceResponse(plugin.ResourceResult{Artifacts: map[string]plugin.Artifact{"installer": installer, "icon": icon}}, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := resourcePlan{Operation: "fixture", Resource: config.Resource{APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Metadata: config.Metadata{Name: "example"}}, Config: json.RawMessage(`{}`), CacheVariants: map[string]string{"icon": variant}}
	run := func(refresh bool) (map[string]Prepared, bool) {
		t.Helper()
		plan.CacheVariants["icon"] = variant
		result, hit, err := prepareResource(t.Context(), store, ops, plan, nil, t.TempDir(), refresh)
		if err != nil {
			t.Fatal(err)
		}
		return result, hit
	}
	portable, hit := run(false)
	if hit {
		t.Fatal("first preparation hit cache")
	}
	variant = "macos-native/1"
	native, hit := run(false)
	if hit || builds != 1 || renders != 2 || native["installer"].Payload != portable["installer"].Payload || native["icon"].Payload == portable["icon"].Payload {
		t.Fatal("renderer transition did not isolate icon preparation")
	}
	if !native["installer"].Cached {
		t.Fatal("installer was not reused")
	}
	if _, hit := run(false); !hit || renders != 2 {
		t.Fatal("native preparation not cached")
	}
	if _, hit := run(true); hit || builds != 1 || renders != 3 {
		t.Fatal("refresh did not exclusively rerender icon")
	}
	variant = "portable/1"
	again, hit := run(false)
	if !hit || again["icon"].Payload != portable["icon"].Payload || builds != 1 {
		t.Fatal("native preparation displaced portable cache")
	}
}
