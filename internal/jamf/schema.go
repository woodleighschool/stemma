package jamf

import (
	"maps"
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/plugin"
)

// MetadataSchema describes package metadata, install policies, patch
// deployment and retention.
func MetadataSchema() *jsonschema.Schema {
	properties := orderedmap.New[string, *jsonschema.Schema]()
	properties.Set("package_id", &jsonschema.Schema{
		Type: "integer", Minimum: "1",
		Description: "Publish into this existing Jamf package instead of discovering one. The package takes Stemma's identity marker, the artifact filename, the artifact's content when it differs, and the declared metadata; a package marked for other software is refused. Omit to find the package by its marker and artifact filename, or create it.",
	})
	properties.Set("category", &jsonschema.Schema{
		AnyOf:       []*jsonschema.Schema{{Type: "string", MinLength: new(uint64(1))}, {Type: "null"}},
		Description: "Category name, which must match exactly one Jamf category. Null removes the category.",
	})
	for _, key := range slices.Sorted(maps.Keys(packageFields)) {
		rule := packageFields[key]
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
	policy := reflector.Reflect(installPolicy{})
	policy.ID, policy.Version = "", ""
	properties.Set("policies", &jsonschema.Schema{
		Type: "array", Items: policy,
		Description: "Install the current package through named policies. Existing policies are updated in place; new policies start disabled and unscoped. Omitted settings remain unchanged.",
	})
	patch := reflector.Reflect(patchConfig{})
	patch.ID, patch.Version = "", ""
	patch.Description = "Link the current package to an existing patch title and optionally manage its policy. Missing version definitions defer the link and target version; other settings on an existing policy still apply. Omitted fields remain unchanged and supplied scope lists replace their collections."
	properties.Set("patch", patch)
	retention := reflector.Reflect(plugin.Retention{})
	retention.ID, retention.Version = "", ""
	retention.Description = "Keep the current package and the N-1 newest others carrying this software's identity marker, ordered by numeric package ID, and delete the rest. A package that a policy, PreStage or patch title still references is kept, and nothing is deleted while those references cannot be read."
	properties.Set("retention", retention)
	return &jsonschema.Schema{
		Type: "object", Properties: properties, AdditionalProperties: jsonschema.FalseSchema,
		Description: "Jamf Pro package metadata. Omitted fields are left unchanged; explicit false, zero and empty strings are managed. Only the documented nullable fields accept null. Each artifact filename has its own package, identified by Stemma's marker in its notes; changed bytes under the same filename are uploaded into that package. Install policies, patch deployment and retention are opt-in.",
	}
}
