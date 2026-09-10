package config

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

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
	descriptor := plugin.Descriptor{Operations: []plugin.Operation{
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
		"core refs":             {`"external":{"installer":"render/artifact","inputs":{"payload":"contents/artifact"}}`, true},
		"unknown field":         {`"external":{"typo":true}`, false},
		"unknown nested field":  {`"external":{"details":{"typo":true}}`, false},
		"invalid literal type":  {`"external":{"enabled":"yes"}`, false},
		"invalid core fact":     {`"external":{"installer":{"$fact":"app.path"}}`, false},
		"other operation":       {`"second":{"label":2}`, true},
		"cross operation field": {`"second":{"title":"Wrong contract"}`, false},
		"unknown connection":    {`"missing":{"title":"Wrong connection"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			document := json.RawMessage(`{"apiVersion":"stemma/v1alpha1","kind":"Software","metadata":{"name":"fixture"},"spec":{"destinations":{` + test.metadata + `}}}`)
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
metadata: {name: fixture}
spec:
  imports: [not-created-yet.yaml]
  destinations:
    external:
      operation: fixture.publish
      config: {token: '${STEMMA_MISSING_SCHEMA_TEST_TOKEN}'}
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
