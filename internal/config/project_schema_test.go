package config

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/internal/macsoftware"
	"github.com/woodleighschool/stemma/plugin"
)

func TestProjectSchemaUsesExternalContractsAndReferenceScopes(t *testing.T) {
	metadata := json.RawMessage(`{
		"$id":"https://fixture.invalid/shared", "$ref":"#/$defs/document",
		"$defs":{
			"document":{"type":"object","additionalProperties":false,"properties":{
				"title":{"type":["string","null"]},"enabled":{"type":"boolean"},
				"details":{"$ref":"#/$defs/details"},"counts":{"type":"array","items":{"type":"integer"}}
			}},
			"details":{"$id":"details","$anchor":"details","type":"object","additionalProperties":false,"properties":{"version":{"$ref":"#/$defs/value"}},"$defs":{"value":{"type":"string"}}},
			"other":{"$id":"other","type":"boolean"}
		}
	}`)
	settings := json.RawMessage(`{"$id":"https://fixture.invalid/shared","$ref":"#/$defs/config","$defs":{"config":{"type":"object","additionalProperties":false,"required":["url"],"properties":{"url":{"type":"string"},"token":{"type":"string","writeOnly":true}}}}}`)
	project := Project{Destinations: map[string]Destination{"external": {Operation: "fixture.publish"}, "second": {Operation: "fixture.other"}}}
	reflector := jsonschema.Reflector{DoNotReference: true}
	spec, err := json.Marshal(reflector.Reflect(macsoftware.Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := plugin.Descriptor{Operations: []plugin.Operation{
		{Name: "fixture.mac", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "stemma/v1alpha1", Kind: "MacSoftware"}, ConfigSchema: spec},
		{Name: "fixture.publish", Kind: "reconcile", MetadataSchema: metadata, ConfigSchema: settings},
		{Name: "fixture.other", Kind: "reconcile", MetadataSchema: json.RawMessage(`{"$id":"https://fixture.invalid/shared","type":"object","properties":{"label":{"type":"integer"}},"additionalProperties":false}`)},
	}}
	schema, err := ProjectSchema(project, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ProjectSchema(project, descriptor)
	if err != nil || !bytes.Equal(schema, again) {
		t.Fatalf("project schema generation changed with identical descriptors: %v", err)
	}
	for name, test := range map[string]struct {
		metadata string
		valid    bool
	}{
		"literal refs":          {"\"external\":{\"title\":null,\"details\":{\"version\":\"1\"},\"counts\":[1]}", true},
		"typed facts":           {`"external":{"enabled":{"$fact":"app.package.has_payload"},"details":{"version":{"$fact":"app.app.version"}},"counts":[{"$fact":"app.size"}]}`, true},
		"core refs":             {`"external":{"installer":"installer","inputs":{"payload":"payload"}}`, true},
		"unknown field":         {`"external":{"typo":true}`, false},
		"unknown nested field":  {`"external":{"details":{"typo":true}}`, false},
		"invalid literal type":  {`"external":{"enabled":"yes"}`, false},
		"invalid core fact":     {`"external":{"installer":{"$fact":"app.path"}}`, false},
		"other operation":       {`"second":{"label":2}`, true},
		"cross operation field": {`"second":{"title":"Wrong contract"}`, false},
		"unknown connection":    {`"missing":{"title":"Wrong connection"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			document := json.RawMessage(`{"apiVersion":"stemma/v1alpha1","kind":"MacSoftware","metadata":{"name":"fixture"},"spec":{"destinations":{` + test.metadata + `}}}`)
			err := plugin.ValidateSchema(schema, document)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, expected %v: %v", err == nil, test.valid, err)
			}
		})
	}
	for name, test := range map[string]struct {
		config string
		valid  bool
	}{
		"native config":    {`{"url":"https://fixture.invalid","token":"${FIXTURE_TOKEN}"}`, true},
		"missing required": {`{}`, false},
		"unknown config":   {`{"url":"https://fixture.invalid","typo":true}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			document := json.RawMessage(`{"apiVersion":"stemma/v1alpha1","kind":"Project","metadata":{"name":"fixture"},"spec":{"imports":["family.yaml"],"destinations":{"external":{"operation":"fixture.publish","config":` + test.config + `}}}}`)
			err := plugin.ValidateSchema(schema, document)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, expected %v: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestSchemaProjectExpandsPluginImagesWithoutLoadingCredentialsOrImports(t *testing.T) {
	t.Setenv("STEMMA_SCHEMA_TEST_IMAGE", "ghcr.io/example/fixture:v1")
	root := t.TempDir()
	writeConfig(t, root, "stemma.yaml", `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: fixture
spec:
  imports:
    - not-created-yet.yaml
  destinations:
    external:
      operation: fixture.publish
      config:
        token: '${STEMMA_MISSING_SCHEMA_TEST_TOKEN}'
  plugins:
    fixture:
      trusted: true
      image: '${STEMMA_SCHEMA_TEST_IMAGE}'
`)
	project, err := LoadSchemaProject(filepath.Join(root, "stemma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if project.Destinations["external"].Config["token"] != "${STEMMA_MISSING_SCHEMA_TEST_TOKEN}" {
		t.Fatal("schema loading expanded credentials")
	}
	if project.Plugins["fixture"].Image != "ghcr.io/example/fixture:v1" {
		t.Fatal("schema loading did not expand the plugin image")
	}
}

func TestProjectSchemaRegistersExternalKindsByGroupVersionAndKind(t *testing.T) {
	first := json.RawMessage(`{"$id":"https://fixture.invalid/spec","$ref":"#/$defs/spec","$defs":{"spec":{"type":"object","additionalProperties":false,"required":["message"],"properties":{"message":{"type":"string"},"settings":{"type":"object","additionalProperties":false,"required":["enabled","count"],"properties":{"enabled":{"type":"boolean"},"count":{"type":"integer"}}}}}}}`)
	second := json.RawMessage(`{"$id":"https://fixture.invalid/spec","type":"object","additionalProperties":false,"required":["count"],"properties":{"count":{"type":"integer"}}}`)
	descriptor := plugin.Descriptor{Operations: []plugin.Operation{
		{Name: "fixture.transform", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "example.test/v2", Kind: "Transform"}, ConfigSchema: first},
		{Name: "fixture.other", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "other.test/v1", Kind: "Transform"}, ConfigSchema: second},
	}}
	schema, err := ProjectSchema(Project{}, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		version, spec string
		valid         bool
	}{
		"first kind":               {"example.test/v2", `{"message":"fixture"}`, true},
		"second kind":              {"other.test/v1", `{"count":2}`, true},
		"version mismatch":         {"example.test/v1", `{"message":"fixture"}`, false},
		"wrong kind schema":        {"example.test/v2", `{"count":2}`, false},
		"unknown field":            {"example.test/v2", `{"message":"fixture","typo":true}`, false},
		"missing required":         {"example.test/v2", `{}`, false},
		"partial inherited config": {"example.test/v2", `{"extends":"base","settings":{"enabled":false}}`, true},
		"invalid inherited value":  {"example.test/v2", `{"extends":"base","settings":{"enabled":"false"}}`, false},
		"unknown inherited value":  {"example.test/v2", `{"extends":"base","settings":{"typo":false}}`, false},
		"empty inheritance":        {"example.test/v2", `{"extends":""}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			document := []byte(`{"apiVersion":"` + test.version + `","kind":"Transform","metadata":{"name":"fixture"},"spec":` + test.spec + `}`)
			if err := plugin.ValidateSchema(schema, document); (err == nil) != test.valid {
				t.Fatalf("valid=%v expected=%v: %v", err == nil, test.valid, err)
			}
		})
	}
}

type releaseSettings struct {
	Major   int    `json:"major" jsonschema:"minimum=1" jsonschema_description:"Major release to track."`
	Channel string `json:"channel,omitempty" jsonschema:"enum=production,enum=preview,default=production" jsonschema_description:"Release channel."`
}

func TestProjectSchemaComposesTypedResolvers(t *testing.T) {
	registry := plugin.New("fixture", "1")
	if err := plugin.Register(registry, plugin.Operation{Name: "fixture.release", Kind: "resolve", Resolver: &plugin.ResolverKind{Version: "1"}, SideEffects: "none", Methods: []string{"validate", "run"}}, func(context.Context, plugin.ResolveRequest[releaseSettings]) (plugin.ResolveResponse, error) {
		t.Fatal("schema generation invoked a resolver")
		return plugin.ResolveResponse{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Register(registry, plugin.Operation{Name: "fixture.resource", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "fixture/v1", Kind: "Application"}, SideEffects: "none", Methods: []string{"validate", "run"}}, func(context.Context, plugin.ResourceRequest[struct {
		Sources []plugin.Input `json:"sources" jsonschema_description:"Inputs to acquire."`
	}]) (plugin.ResourceResult, error) {
		return plugin.ResourceResult{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	schema, err := ProjectSchema(Project{}, registry.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		source string
		valid  bool
	}{
		{`{"resolver":"fixture.release","major":4}`, true},
		{`{"resolver":"fixture.release","major":4,"channel":"preview"}`, true},
		{`{"resolver":"fixture.release","major":4,"channel":"${RELEASE_CHANNEL}"}`, true},
		{`{"resolver":"fixture.release"}`, false},
		{`{"resolver":"fixture.release","major":0}`, false},
		{`{"resolver":"fixture.release","major":4,"channel":"stable"}`, false},
		{`{"resolver":"fixture.release","major":4,"typo":true}`, false},
		{`{"resolver":"unloaded.release","major":4}`, false},
	} {
		document := []byte(`{"apiVersion":"fixture/v1","kind":"Application","metadata":{"name":"app"},"spec":{"sources":[` + test.source + `]}}`)
		if err := plugin.ValidateSchema(schema, document); (err == nil) != test.valid {
			t.Fatalf("%s valid=%v: %v", test.source, test.valid, err)
		}
	}
	var document map[string]any
	if err := json.Unmarshal(schema, &document); err != nil {
		t.Fatal(err)
	}
	var found bool
	walkEditorSchema(document, func(node map[string]any) {
		properties, _ := node["properties"].(map[string]any)
		resolver, _ := properties["resolver"].(map[string]any)
		if resolver["const"] != "fixture.release" || node["required"] == nil {
			return
		}
		major := properties["major"].(map[string]any)
		channel := properties["channel"].(map[string]any)
		if major["description"] != "Major release to track." || channel["default"] != "production" {
			t.Fatal("editor lost config annotations")
		}
		found = true
	})
	if !found {
		t.Fatal("typed resolver schema missing from editor")
	}
}
