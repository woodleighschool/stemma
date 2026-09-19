package jamf

import (
	"maps"
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/plugin"
)

// MetadataSchema describes native package metadata, patch deployment and retention.
func MetadataSchema() *jsonschema.Schema {
	properties := orderedmap.New[string, *jsonschema.Schema]()
	properties.Set("package_id", &jsonschema.Schema{
		Type: "string", Pattern: "^[1-9][0-9]*$", MaxLength: new(uint64(20)),
		Description: "Publish into this existing Jamf package ID instead of discovering one. The package takes Stemma's identity marker, the artifact filename, the artifact's content when it differs, and the declared metadata; a package marked for other software is refused. Omit to find the package by its marker and artifact filename, or create it.",
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
	reflector := &jsonschema.Reflector{DoNotReference: true}
	patch := reflector.Reflect(patchConfig{})
	patch.ID, patch.Version = "", ""
	patch.Description = "Associate the package with an exact version in an existing patch title, replacing that version's package and keeping other versions. An optional native patch policy is found by id, or by name within the title, where the name defaults to the software name; it is updated in place, or created disabled and unscoped before supplied settings are applied. Omitted fields are left unchanged and supplied scope lists replace their collections. Without patch, title associations and policies are left as they are."
	properties.Set("patch", patch)
	retention := reflector.Reflect(plugin.Retention{})
	retention.ID, retention.Version = "", ""
	retention.Description = "Keep the current package and the N-1 newest others carrying this software's identity marker, ordered by numeric package ID, and delete the rest. A package that a policy, PreStage or patch title still references is kept, and nothing is deleted while those references cannot be read."
	properties.Set("retention", retention)
	return &jsonschema.Schema{
		Type: "object", Properties: properties, AdditionalProperties: jsonschema.FalseSchema,
		Description: "Native Jamf Pro v1 package metadata. Omitted fields are left unchanged; explicit false, zero and empty strings are managed. Only the documented nullable strings accept null. Each artifact filename has its own package, named natively and identified by Stemma's marker in its notes; changed bytes under the same filename are uploaded into that package. Patch deployment and retention are opt-in.",
	}
}
