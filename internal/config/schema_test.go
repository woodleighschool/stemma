package config

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/internal/macpkg"
	"github.com/woodleighschool/stemma/internal/macsoftware"
	"github.com/woodleighschool/stemma/internal/windowssoftware"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

func TestResourceEditorSchema(t *testing.T) {
	schema, err := testCatalogSchema()
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		kind, spec string
		valid      bool
	}{
		"vendor mac":                {"MacSoftware", `{"source":{"url":"https://example.test/app.pkg"},"application":{"bundle_id":"org.example.app"}}`, true},
		"HTTP headers":              {"MacSoftware", `{"source":{"url":"https://example.test/download","headers":{"User-Agent":"Fixture"},"filename":"App.pkg"}}`, true},
		"HTTP header value":         {"MacSoftware", `{"source":{"url":"https://example.test/download","headers":{"Accept":["one","two"]}}}`, false},
		"HTTP unknown option":       {"MacSoftware", `{"source":{"url":"https://example.test/download","curl_opts":[]}}`, false},
		"HTTP flat resolver":        {"MacSoftware", `{"source":{"resolver":"http","url":"https://example.test/download","headers":{"Accept":"application/zip"}}}`, true},
		"file mac":                  {"MacSoftware", `{"source":{"path":"../Shared/app.pkg"}}`, true},
		"external resolver":         {"MacSoftware", `{"source":{"resolver":"example.release","channel":"stable"}}`, true},
		"resource source":           {"MacSoftware", `{"source":{"resource":{"kind":"BuildMacPkg","name":"branding"}}}`, true},
		"source free mac":           {"MacSoftware", `{"destinations":{"external":{"title":"Policy"}}}`, true},
		"build":                     {"BuildMacPkg", `{"package":{"identifier":"org.example.payload","version":"1"},"inputs":{"text":{"path":"text.txt"}},"payload":{"/Library/Example/text.txt":{"$input":"text","mode":"0644"}}}`, true},
		"windows":                   {"WindowsSoftware", `{"source":{"path":"setup.exe"},"content":{"files":{"config.xml":{"path":"config.xml"}}},"destinations":{"external":{"display_name":"Fixture"}}}`, true},
		"inherited build":           {"BuildMacPkg", `{"extends":"base","payload":{"/Library/Example/text.txt":{"content":"text"}}}`, true},
		"inherited windows":         {"WindowsSoftware", `{"extends":"base"}`, true},
		"inherited source override": {"MacSoftware", `{"extends":"base","source":{"path":"different.pkg"}}`, true},
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
	for value, valid := range map[string]bool{"true": true, "false": true, `"yes"`: false} {
		document := []byte(`{"apiVersion":"stemma/v1alpha1","kind":"MacSoftware","metadata":{"name":"fixture"},"suspend":` + value + `,"spec":{"destinations":{"external":{"title":"Policy"}}}}`)
		if err := plugin.ValidateSchema(schema, document); (err == nil) != valid {
			t.Fatalf("suspend=%s valid=%v: %v", value, err == nil, err)
		}
	}
}

func TestProjectEnvelopeSchema(t *testing.T) {
	schema, err := baseSchema()
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

func testCatalogSchema() ([]byte, error) {
	encode := func(value any) json.RawMessage { data, _ := json.Marshal(value); return data }
	return ProjectSchema(Project{Destinations: map[string]Destination{"external": {Operation: "fixture.publish"}}}, plugin.Descriptor{Operations: []plugin.Operation{
		{Name: "fixture.publish", Kind: "reconcile"},
		{Name: "example.release", Resolver: &plugin.ResolverKind{Version: "1"}, ConfigSchema: encode(plugin.SchemaFor[struct {
			Channel string `json:"channel"`
		}]())},
		{Name: "build.mac.pkg", Resource: &plugin.ResourceKind{APIVersion: "stemma/v1alpha1", Kind: "BuildMacPkg"}, ConfigSchema: encode(plugin.SchemaFor[macpkg.Spec]())},
		{Name: "software.mac", Resource: &plugin.ResourceKind{APIVersion: "stemma/v1alpha1", Kind: "MacSoftware"}, ConfigSchema: encode(plugin.SchemaFor[macsoftware.Spec]())},
		{Name: "software.windows", Resource: &plugin.ResourceKind{APIVersion: "stemma/v1alpha1", Kind: "WindowsSoftware"}, ConfigSchema: encode(plugin.SchemaFor[windowssoftware.Spec]())},
	}})
}
