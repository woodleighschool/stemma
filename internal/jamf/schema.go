package jamf

import (
	"maps"
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
)

// MetadataSchema describes supported native package fields and recipe adoption.
func MetadataSchema() *jsonschema.Schema {
	properties := orderedmap.New[string, *jsonschema.Schema]()
	properties.Set("package_id", &jsonschema.Schema{
		Type: "string", Pattern: "^[1-9][0-9]*$", MaxLength: new(uint64(20)),
		Description: "Adopt this existing Jamf package ID for this recipe. Must match any saved binding. Omit to discover by Stemma's stable filename marker or create a package.",
	})
	for _, key := range slices.Sorted(maps.Keys(managedFields)) {
		rule := managedFields[key]
		kind := rule.kind
		switch kind {
		case "bool":
			kind = "boolean"
		case "int":
			kind = "integer"
		}
		field := &jsonschema.Schema{Type: kind, Description: rule.description}
		if rule.nullable {
			field.Type = ""
			field.AnyOf = []*jsonschema.Schema{{Type: kind}, {Type: "null"}}
		}
		properties.Set(key, field)
	}
	return &jsonschema.Schema{
		Type: "object", Properties: properties, AdditionalProperties: jsonschema.FalseSchema,
		Description: "Native Jamf Pro v1 package metadata. Omitted fields are unmanaged; explicit false, zero and empty strings are managed. Only the documented nullable strings accept null. Policies, scope and assignments are unsupported. Content updates retain the package ID and can affect policies that already reference it.",
	}
}

// ConnectionSchema describes shared Jamf credentials without recipe adoption.
func ConnectionSchema() *jsonschema.Schema {
	reflector := &jsonschema.Reflector{DoNotReference: true}
	schema := reflector.Reflect(configuration{})
	schema.ID, schema.Version = "", ""
	schema.Description = "Shared Jamf Pro connection using client credentials and the v1 package API reviewed against Jamf Pro 11.31. Requires package read/write/upload privileges and a distribution configuration that supports package upload. Put package_id in recipe metadata to adopt an existing package."
	return schema
}
