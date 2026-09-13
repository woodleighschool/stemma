package source

import (
	"reflect"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/plugin"
)

// InputSchema describes built-in HTTP configuration while retaining extensible
// resolver inputs. It is a jsonschema.Reflector mapper.
func InputSchema(t reflect.Type) *jsonschema.Schema {
	if t != reflect.TypeFor[plugin.Input]() {
		return nil
	}
	schema := (plugin.Input{}).JSONSchema()
	http := (&jsonschema.Reflector{DoNotReference: true}).Reflect(nativeConfig{})
	http.Version = ""
	http.ID = ""
	for _, name := range []string{"include", "base", "path", "repository", "release", "asset"} {
		http.Properties.Delete(name)
	}
	http.Required = []string{"url"}
	if token, ok := http.Properties.Get("token"); ok {
		token.WriteOnly = true
	}
	schema.AllOf = []*jsonschema.Schema{
		{
			If:   &jsonschema.Schema{Required: []string{"url"}, Not: &jsonschema.Schema{Required: []string{"resolver"}}},
			Then: http,
		},
	}
	explicit := &jsonschema.Schema{Type: "object", Required: []string{"resolver"}, Properties: orderedmap.New[string, *jsonschema.Schema]()}
	explicit.Properties.Set("resolver", &jsonschema.Schema{Const: "http"})
	wrapped := &jsonschema.Schema{Type: "object", AdditionalProperties: jsonschema.FalseSchema, Properties: orderedmap.New[string, *jsonschema.Schema]()}
	wrapped.Properties.Set("resolver", &jsonschema.Schema{Const: "http"})
	wrapped.Properties.Set("config", http)
	flat := *http
	flat.Properties = orderedmap.New[string, *jsonschema.Schema]()
	for name, property := range http.Properties.FromOldest() {
		flat.Properties.Set(name, property)
	}
	flat.Properties.Set("resolver", &jsonschema.Schema{Const: "http"})
	schema.AllOf = append(schema.AllOf, &jsonschema.Schema{
		If: explicit,
		Then: &jsonschema.Schema{
			If: &jsonschema.Schema{Required: []string{"config"}}, Then: wrapped, Else: &flat,
		},
	})
	return schema
}
