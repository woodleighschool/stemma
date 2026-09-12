package plugin

import (
	"encoding/json"
	"github.com/invopop/jsonschema"
)

// JSONSchema keeps resolver declarations extensible while distinguishing resource references.
func (Input) JSONSchema() *jsonschema.Schema {
	var schema jsonschema.Schema
	_ = json.Unmarshal([]byte(`{"type":"object","oneOf":[
 {"required":["resource"],"additionalProperties":false,"properties":{"resource":{"type":"object","additionalProperties":false,"required":["kind","name"],"properties":{"apiVersion":{"type":"string"},"kind":{"type":"string"},"name":{"type":"string"},"output":{"type":"string"}}}}},
 {"not":{"required":["resource"]},"anyOf":[{"required":["url"]},{"required":["path"]},{"required":["resolver"]}]}
 ]}`), &schema)
	return &schema
}
