package source

import (
	"reflect"
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/plugin"
)

// InputSchema describes native inputs. The catalog composer adds installed resolvers.
func InputSchema(t reflect.Type) *jsonschema.Schema {
	if t != reflect.TypeFor[plugin.Input]() {
		return nil
	}
	schema := (plugin.Input{}).JSONSchema()
	schema.OneOf = schema.OneOf[:1]
	schema.Extras = nil
	for _, resolver := range []string{"http", "github", "file", "local"} {
		config := (&jsonschema.Reflector{DoNotReference: true}).Reflect(nativeConfig{})
		config.Version, config.ID = "", ""
		var remove []string
		for name := range config.Properties.FromOldest() {
			if !slices.Contains(nativeFields[resolver], name) {
				remove = append(remove, name)
			}
		}
		for _, name := range remove {
			config.Properties.Delete(name)
		}
		switch resolver {
		case "http":
			config.Required = []string{"url"}
		case "github":
			config.Required = []string{"repository", "asset"}
		case "file":
			config.Required = []string{"path"}
		case "local":
			config.Required = []string{"include"}
		}
		for _, name := range config.Required {
			property, _ := config.Properties.Get(name)
			if property.Type == "string" {
				property.MinLength = new(uint64(1))
			}
			if property.Type == "array" {
				property.MinItems = new(uint64(1))
			}
		}
		if token, ok := config.Properties.Get("token"); ok {
			token.WriteOnly = true
		}
		if resolver == "http" || resolver == "file" {
			schema.OneOf = append(schema.OneOf, config)
		}
		explicit := *config
		explicit.Properties = orderedmap.New[string, *jsonschema.Schema]()
		for name, property := range config.Properties.FromOldest() {
			explicit.Properties.Set(name, property)
		}
		explicit.Properties.Set("resolver", &jsonschema.Schema{Const: resolver, Description: "Built-in resolver selecting this input."})
		explicit.Required = append(slices.Clone(config.Required), "resolver")
		schema.OneOf = append(schema.OneOf, &explicit)
	}
	return schema
}
