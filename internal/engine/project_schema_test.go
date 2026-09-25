package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/plugin"
)

func TestBuiltinSchemaDescribesUnboundDestinations(t *testing.T) {
	schema, err := BuiltinSchema()
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		spec  string
		valid bool
	}{
		"named destination": {`{"destinations":{"school":{"pkginfo":{"catalogs":["testing"]}}}}`, true},
		"unknown metadata":  {`{"destinations":{"school":{"typo":true}}}`, false},
		"invalid name":      {`{"destinations":{"bad name":{}}}`, false},
		"native input":      {`{"source":{"url":"https://example.test/app.pkg"}}`, true},
		"external resolver": {`{"source":{"resolver":"blender","major":4}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			document := []byte(`{"apiVersion":"stemma/v1alpha1","kind":"MacSoftware","metadata":{"name":"fixture"},"spec":` + test.spec + `}`)
			if err := plugin.ValidateSchema(schema, document); (err == nil) != test.valid {
				t.Fatalf("valid=%v expected=%v: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestProjectSchemaOnlyDescribesOperations(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	if err := os.WriteFile(filename, []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: fixture
spec:
  imports:
    - not-created-yet.yaml
  destinations:
    deployment:
      operation: intune
      config:
        tenant_id: '{{ env.STEMMA_MISSING_SCHEMA_TENANT }}'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ProjectSchema(t.Context(), Options{ConfigPath: filename, Lock: lockfile.Options{Offline: true}, Handlers: map[string]reconcileHandler{"intune": func(context.Context, plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		t.Fatal("schema generation invoked a destination")
		return plugin.ReconcileResponse{}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("missing project schema")
	}
}

type validationConfig struct {
	Major int `json:"major" jsonschema:"minimum=1"`
}

func (c validationConfig) Validate() error {
	if c.Major == 2 {
		return errors.New("major 2 is no longer published")
	}
	return nil
}

func TestDiscoveryValidatesResolverConfigBeforeAcquisition(t *testing.T) {
	ops, err := builtins(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.Register(ops.registry, plugin.Operation{Name: "fixture.release", Kind: "resolve", Resolver: &plugin.ResolverKind{Version: "1"}, SideEffects: "none", Methods: []string{"validate", "discover", "run"}}, func(context.Context, plugin.ResolveRequest[validationConfig]) (plugin.ResolveResponse, error) {
		t.Fatal("validation acquired content")
		return plugin.ResolveResponse{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		resolver string
		settings map[string]any
		message  string
	}{
		{"fixture.release", map[string]any{"major": 4}, ""},
		{"fixture.release", map[string]any{}, "major"},
		{"fixture.release", map[string]any{"major": 2}, "no longer published"},
		{"fixture.release", map[string]any{"major": 4, "typo": true}, "typo"},
		{"not.loaded", map[string]any{}, "unknown resolver"},
	} {
		input := maps.Clone(test.settings)
		input["resolver"] = test.resolver
		project := config.Project{Resources: map[string]config.Resource{"app": {APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Metadata: config.Metadata{Name: "app"}, Spec: map[string]any{"source": input}}}}
		_, _, err := discoverClosure(t.Context(), project, ops, sortedKeys(project.Resources))
		if test.message == "" && err != nil || test.message != "" && (err == nil || !strings.Contains(err.Error(), test.message)) {
			t.Fatalf("%s %v: %v", test.resolver, test.settings, err)
		}
	}
}
