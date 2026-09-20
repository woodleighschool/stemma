package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

type architecture string

const (
	arm64 architecture = "arm64"
	x64   architecture = "x64"
)

func (architecture) JSONSchemaExtend(s *jsonschema.Schema) { s.Enum = []any{arm64, x64} }

type nestedConfig struct {
	Name    string `json:"name" jsonschema_description:"Human-readable item name."`
	Enabled bool   `json:"enabled,omitempty" jsonschema:"default=true"`
}
type resolverConfig struct {
	Major        int            `json:"major" jsonschema:"minimum=1" jsonschema_description:"Major release to track."`
	Architecture architecture   `json:"architecture,omitempty" jsonschema:"default=arm64"`
	Count        int            `json:"count,omitempty" jsonschema:"default=7"`
	Label        string         `json:"label,omitempty" jsonschema:"default=release"`
	Items        []nestedConfig `json:"items,omitempty"`
	Nested       *nestedConfig  `json:"nested,omitempty"`
}

func (c resolverConfig) Validate() error {
	if c.Major == 1 && c.Architecture == x64 {
		return errors.New("major 1 does not publish x64 installers")
	}
	return nil
}

func TestTypedRegistration(t *testing.T) {
	var received resolverConfig
	calls := 0
	register := func() *plugin.Registry {
		registry := plugin.New("fixture", "1")
		err := plugin.Register(registry, plugin.Operation{Name: "release", Kind: "resolve", Resolver: &plugin.ResolverKind{Version: "1"}, SideEffects: "none", Methods: []string{"validate", "run"}}, func(_ context.Context, request plugin.ResolveRequest[resolverConfig]) (resolverConfig, error) {
			calls++
			received = request.Config
			return request.Config, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return registry
	}
	registry := register()
	call := func(method, config string) error {
		_, err := registry.Handle(t.Context(), plugin.Request{Protocol: plugin.ProtocolVersion, Operation: "release", Method: method, Input: json.RawMessage(`{"config":` + config + `}`)})
		return err
	}
	if err := call("run", `{"major":4,"items":[{"name":"one"},{"name":"two","enabled":false}],"nested":{"name":"child"}}`); err != nil {
		t.Fatal(err)
	}
	want := resolverConfig{Major: 4, Architecture: arm64, Count: 7, Label: "release", Items: []nestedConfig{{Name: "one", Enabled: true}, {Name: "two", Enabled: false}}, Nested: &nestedConfig{Name: "child", Enabled: true}}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("effective config = %+v, want %+v", received, want)
	}
	if err := call("run", `{"major":4,"count":0,"label":""}`); err != nil {
		t.Fatal(err)
	}
	if received.Count != 0 || received.Label != "" || received.Nested != nil {
		t.Fatalf("explicit zero or absent nested config changed: %+v", received)
	}
	for _, test := range []struct{ config, message string }{
		{`{}`, "major"}, {`{"major":0}`, "minimum"}, {`{"major":4,"extra":true}`, "extra"},
		{`{"major":4,"architecture":"mips"}`, "arm64"}, {`{"major":4,"architecture":null}`, "null"},
		{`{"major":4,"nested":{"name":"child","typo":false}}`, "typo"},
		{`{"major":1,"architecture":"x64"}`, "major 1 does not publish x64"},
	} {
		t.Run(test.config, func(t *testing.T) {
			before := calls
			for _, method := range []string{"validate", "run"} {
				if err := call(method, test.config); err == nil || !strings.Contains(err.Error(), test.message) || !strings.Contains(err.Error(), "release") {
					t.Fatalf("error = %v, want useful %q error", err, test.message)
				}
			}
			if calls != before {
				t.Fatal("invalid config reached resolver")
			}
		})
	}
	before := calls
	if err := call("validate", `{"major":1}`); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("validation invoked resolver")
	}
	first, _ := json.Marshal(registry.Descriptor())
	second, _ := json.Marshal(register().Descriptor())
	if string(first) != string(second) {
		t.Fatal("identical type registrations produced different contracts")
	}
	schema := registry.Descriptor().Operations[0].ConfigSchema
	for _, fragment := range []string{`"required":["major"]`, `"default":"arm64"`, `"enum":["arm64","x64"]`, `"additionalProperties":false`, `"description":"Major release to track."`} {
		if !strings.Contains(string(schema), fragment) {
			t.Fatalf("missing schema contract %s in %s", fragment, schema)
		}
	}
	// Microsoft-like semantic rules remain runtime-authoritative.
	if err := plugin.ValidateSchema(schema, []byte(`{"major":1,"architecture":"x64"}`)); err != nil {
		t.Fatalf("structural schema unexpectedly encoded semantics: %v", err)
	}
}

type checkedConfig struct {
	Name string `json:"name"`
}

func (c *checkedConfig) Validate() error {
	if c.Name == "blocked" {
		return errors.New("name is blocked")
	}
	return nil
}

func TestResourceDeclarationValidation(t *testing.T) {
	registry := plugin.New("fixture", "1")
	calls := 0
	operation := plugin.Operation{Name: "fixture.resource", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "fixture/v1", Kind: "Item"}, SideEffects: "none", Methods: []string{"discover", "run"}}
	if err := plugin.Register(registry, operation, func(_ context.Context, _ plugin.ResourceRequest[checkedConfig]) (plugin.ResourceResult, error) {
		calls++
		return plugin.ResourceResult{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"discover", "run"} {
		data, _ := json.Marshal(plugin.ResourceRequest[checkedConfig]{Identity: plugin.ResourceReference{Kind: "Item", Name: "example"}, Config: checkedConfig{Name: "blocked"}})
		_, err := registry.Handle(t.Context(), plugin.Request{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: method, Input: data})
		if method == "discover" && (err == nil || !strings.Contains(err.Error(), "name is blocked")) {
			t.Fatalf("semantic validation: %v", err)
		}
		if method == "run" && err != nil {
			t.Fatalf("prepared config was checked as a declaration: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("handler called %d times", calls)
	}
}
