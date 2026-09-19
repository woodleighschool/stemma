package plugin

import "github.com/invopop/jsonschema"

// JSONSchema distinguishes resource references from dynamic resolver declarations.
// The catalog composer binds the latter to the loaded registry.
func (Input) JSONSchema() *jsonschema.Schema {
	r := jsonschema.Reflector{DoNotReference: true}
	reference := r.Reflect(struct {
		Resource ResourceOutputReference `json:"resource" jsonschema_description:"Use a named output from another resource in this project."`
	}{})
	reference.ID, reference.Version = "", ""
	return &jsonschema.Schema{
		Type: "object",
		OneOf: []*jsonschema.Schema{reference, {
			Not:   &jsonschema.Schema{Required: []string{"resource"}},
			AnyOf: []*jsonschema.Schema{{Required: []string{"url"}}, {Required: []string{"path"}}, {Required: []string{"resolver"}}},
		}},
		Extras: map[string]any{"x-stemma-input": true},
	}
}
