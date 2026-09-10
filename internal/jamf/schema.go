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
		Description: "Adopt this existing Jamf package ID for the selected immutable payload. Existing content must match. Omit to discover by the artifact filename marker or create a package.",
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
	patch.Description = "Associate the immutable package with an exact version in an existing patch title. An optional native patch policy retains its ID across versions; new policies start disabled and unscoped before supplied settings are applied. Omitted fields remain unmanaged and supplied scope lists replace their collections."
	properties.Set("patch", patch)
	retention := reflector.Reflect(plugin.Retention{})
	retention.ID, retention.Version = "", ""
	retention.Description = "Keep the current payload and the N-1 most recently published distinct payloads, plus referenced packages. Only owned packages with known publication order can be deleted. Unchanged owned title-version associations outside retention are retired only when no patch policy targets that version."
	properties.Set("retention", retention)
	return &jsonschema.Schema{
		Type: "object", Properties: properties, AdditionalProperties: jsonschema.FalseSchema,
		Description: "Native Jamf Pro v1 package metadata. Omitted fields are unmanaged; explicit false, zero and empty strings are managed. Only the documented nullable strings accept null. Each distinct payload has an immutable package ID. Patch deployment and retention are opt-in.",
	}
}

// ConnectionSchema describes shared Jamf credentials without software adoption.
func ConnectionSchema() *jsonschema.Schema {
	reflector := &jsonschema.Reflector{DoNotReference: true}
	schema := reflector.Reflect(configuration{})
	schema.ID, schema.Version = "", ""
	if secret, ok := schema.Properties.Get("client_secret"); ok {
		secret.WriteOnly = true
	}
	schema.Description = "Shared Jamf Pro connection using client credentials and the v1 package API reviewed against Jamf Pro 11.31. Requires package read/write/upload privileges and a distribution configuration that supports package upload. Put package_id in software metadata to adopt an existing package."
	return schema
}
