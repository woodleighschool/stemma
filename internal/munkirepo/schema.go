package munkirepo

import (
	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/internal/munki"
)

// MetadataSchema describes the native repository publication settings.
func MetadataSchema() *jsonschema.Schema { return munki.DestinationSchema() }
