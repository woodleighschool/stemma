package plugin

import (
	"encoding/json"
	"github.com/invopop/jsonschema"
)

// JSONSchema keeps resolver declarations extensible while distinguishing resource references.
func (Input) JSONSchema() *jsonschema.Schema {
	var schema jsonschema.Schema
	_ = json.Unmarshal([]byte(`{"type":"object","oneOf":[
 {"required":["resource"],"additionalProperties":false,"properties":{"resource":{}}},
 {"not":{"required":["resource"]},"anyOf":[{"required":["url"]},{"required":["path"]},{"required":["resolver"]}]}
 ]}`), &schema)
	r := jsonschema.Reflector{DoNotReference: true}
	reference := r.Reflect(ResourceOutputReference{})
	reference.ID = ""
	schema.OneOf[0].Properties.Set("resource", reference)
	return &schema
}
