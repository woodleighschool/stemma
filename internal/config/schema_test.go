package config

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

func TestSchemaHasCurrentResourceContracts(t *testing.T) {
	data, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	definitions := schema["$defs"].(map[string]any)
	for _, kind := range []string{"BuildMacPkg", "MacSoftware", "WindowsSoftware"} {
		if definitions[kind] == nil {
			t.Errorf("missing %s contract", kind)
		}
	}
	for _, name := range []string{"SoftwareDocument", "Step", "MunkiMetadata", "JamfMetadata", "IntuneMetadata"} {
		if definitions[name] != nil {
			t.Errorf("core schema retained provider or removed resource definition %s", name)
		}
	}
	for name, fields := range map[string][]string{"Metadata": {"name"}, "ProjectSpec": {"imports", "components"}, "Destination": {"operation", "config"}} {
		properties := definitions[name].(map[string]any)["properties"].(map[string]any)
		for _, field := range fields {
			if properties[field].(map[string]any)["description"] == "" {
				t.Errorf("%s.%s lacks an editor description", name, field)
			}
		}
	}
}

func TestResourceEditorSchema(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		kind, spec string
		valid      bool
	}{
		"vendor mac":                {"MacSoftware", `{"source":{"url":"https://example.test/app.pkg"},"application":{"bundle_id":"org.example.app"}}`, true},
		"file mac":                  {"MacSoftware", `{"source":{"path":"../Shared/app.pkg"}}`, true},
		"external resolver":         {"MacSoftware", `{"source":{"resolver":"example.release","config":{"channel":"stable"}}}`, true},
		"resource source":           {"MacSoftware", `{"source":{"resource":{"kind":"BuildMacPkg","name":"branding"}}}`, true},
		"source free mac":           {"MacSoftware", `{"destinations":{"external":{"title":"Policy"}}}`, true},
		"build":                     {"BuildMacPkg", `{"package":{"identifier":"org.example.payload","version":"1"},"inputs":{"text":{"path":"text.txt"}},"payload":{"/Library/Example/text.txt":{"$input":"text","mode":"0644"}}}`, true},
		"windows":                   {"WindowsSoftware", `{"source":{"path":"setup.exe"},"content":{"files":{"config.xml":{"path":"config.xml"}}},"destinations":{"external":{"displayName":"Fixture"}}}`, true},
		"inherited build":           {"BuildMacPkg", `{"extends":"base","payload":{"/Library/Example/text.txt":{"content":"text"}}}`, true},
		"inherited windows":         {"WindowsSoftware", `{"extends":"base"}`, true},
		"inherited source override": {"MacSoftware", `{"extends":"base","source":{"path":"different.pkg"}}`, true},
		"old steps":                 {"MacSoftware", `{"steps":[{"operation":"pkg"}]}`, false},
		"unknown resource field":    {"MacSoftware", `{"typo":true}`, false},
		"unknown nested field":      {"MacSoftware", `{"application":{"typo":true}}`, false},
		"missing package":           {"BuildMacPkg", `{"payload":{}}`, false},
		"missing vendor source":     {"WindowsSoftware", `{"destinations":{"external":{}}}`, false},
		"mixed resource input":      {"MacSoftware", `{"source":{"resource":{"kind":"BuildMacPkg","name":"fixture"},"url":"https://example.test/app.pkg"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			document := []byte(`{"apiVersion":"stemma/v1alpha1","kind":"` + test.kind + `","metadata":{"name":"fixture"},"spec":` + test.spec + `}`)
			if err := plugin.ValidateSchema(schema, document); (err == nil) != test.valid {
				t.Fatalf("valid=%v expected=%v: %v", err == nil, test.valid, err)
			}
		})
	}
	if err := plugin.ValidateSchema(schema, []byte(`{"apiVersion":"example.test/v1","kind":"Transform","metadata":{"name":"fixture"},"spec":{}}`)); err == nil {
		t.Fatal("base schema accepted an unregistered external kind")
	}
}

func TestProjectEnvelopeSchema(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := yaml.Unmarshal([]byte(projectFixture), &value); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateSchema(schema, encoded); err != nil {
		t.Fatal(err)
	}
}
