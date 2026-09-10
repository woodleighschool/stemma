package munkirepo

import (
	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/internal/munki"
)

// MetadataSchema describes native repository publication authoring.
func MetadataSchema() *jsonschema.Schema { return munki.DestinationSchema() }

// ConnectionSchema describes the local repository location.
func ConnectionSchema() *jsonschema.Schema {
	r := jsonschema.Reflector{DoNotReference: true, RequiredFromJSONSchemaTags: true}
	schema := r.Reflect(&struct {
		Path string `json:"path" jsonschema:"required,minLength=1,description=Repository path relative to the project root or an absolute path."`
	}{})
	schema.ID = ""
	return schema
}
