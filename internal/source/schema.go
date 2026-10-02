package source

import (
	"reflect"
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/plugin"
)

// ResolverSchema returns the schema owned by a built-in registration.
func ResolverSchema(name string) *jsonschema.Schema {
	return builtinDescriptors[name].Schema
}

// InputSchema describes native inputs. The catalog composer adds installed resolvers.
func InputSchema(t reflect.Type) *jsonschema.Schema {
	if t != reflect.TypeFor[plugin.Input]() {
		return nil
	}
	schema := (plugin.Input{}).JSONSchema()
	schema.OneOf = schema.OneOf[:1]
	schema.Extras = nil
	for _, resolver := range Resolvers() {
		config := ResolverSchema(resolver)
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
