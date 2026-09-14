package source

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

func TestNativeInputSchemaAndValidation(t *testing.T) {
	schema, err := json.Marshal(InputSchema(reflect.TypeFor[plugin.Input]()))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, declaration string
		valid             bool
	}{
		{"GitHub glob", `{"resolver":"github","repository":"company/application","asset":"Application-*-arm64.zip"}`, true},
		{"GitHub wrapped", `{"resolver":"github","config":{"repository":"company/application","release":"latest","asset":"App.pkg"}}`, true},
		{"GitHub missing asset", `{"resolver":"github","repository":"company/application"}`, false},
		{"GitHub empty asset", `{"resolver":"github","repository":"company/application","asset":""}`, false},
		{"GitHub foreign regex", `{"resolver":"github","repository":"company/application","asset":"App.pkg","match":""}`, false},
		{"GitHub wrapped foreign", `{"resolver":"github","config":{"repository":"company/application","asset":"App.pkg","headers":{}}}`, false},
		{"GitHub mixed", `{"resolver":"github","config":{"repository":"company/application","asset":"App.pkg"},"release":"latest"}`, false},
		{"HTTP regex", `{"resolver":"http","url":"https://example.test/download","match":"https://example[.]test/App-[0-9]+[.]pkg"}`, true},
		{"HTTP foreign", `{"url":"https://example.test/download","asset":null}`, false},
		{"local globs", `{"resolver":"local","include":["**/*.ttf","**/*.otf"]}`, true},
		{"local empty", `{"resolver":"local","include":[]}`, false},
		{"local foreign", `{"resolver":"local","include":["*.ttf"],"token":""}`, false},
		{"file exact", `{"path":"Assets/App.pkg"}`, true},
		{"file wrapped", `{"resolver":"file","config":{"path":"Assets/App.pkg"}}`, true},
		{"file foreign", `{"path":"Assets/App.pkg","release":""}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(test.declaration)
			if err := plugin.ValidateSchema(schema, data); (err == nil) != test.valid {
				t.Errorf("schema: %v, want valid=%v", err, test.valid)
			}
			var input plugin.Input
			err := json.Unmarshal(data, &input)
			if err == nil {
				_, err = native(input)
			}
			if (err == nil) != test.valid {
				t.Errorf("runtime: %v, want valid=%v", err, test.valid)
			}
		})
	}
}

func TestSourceDocumentationExamples(t *testing.T) {
	data, err := os.ReadFile("../../docs/content/sources.md")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := json.Marshal(InputSchema(reflect.TypeFor[plugin.Input]()))
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```yaml\\n(.*?)```").FindAllSubmatch(data, -1)
	if len(blocks) == 0 {
		t.Fatal("no source examples")
	}
	for _, block := range blocks {
		var document struct {
			Source map[string]any `yaml:"source"`
		}
		if err := yaml.Unmarshal(block[1], &document); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(document.Source)
		if err != nil {
			t.Fatal(err)
		}
		if err := plugin.ValidateSchema(schema, encoded); err != nil {
			t.Fatalf("example schema: %v", err)
		}
		var input plugin.Input
		if err := json.Unmarshal(encoded, &input); err != nil {
			t.Fatal(err)
		}
		if input.Resource == nil {
			if _, err := native(input); err != nil {
				t.Fatalf("example runtime: %v", err)
			}
		}
	}
}
