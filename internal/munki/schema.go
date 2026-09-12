package munki

import "github.com/invopop/jsonschema"

// MetadataSchema describes the native writable fields shared by configuration,
// rendering operations and repository publication.
func MetadataSchema() *jsonschema.Schema {
	metadataReflector := &jsonschema.Reflector{DoNotReference: true, RequiredFromJSONSchemaTags: true}
	metadata := metadataReflector.Reflect(&Metadata{})
	metadata.ID = ""
	metadata.Description = "Native Munki metadata. Omitted fields remain unmanaged; explicit lists own the whole collection."
	for _, name := range []string{"display_name", "description", "category", "developer", "icon_name", "icon_hash"} {
		field, _ := metadata.Properties.Get(name)
		metadata.Properties.Set(name, &jsonschema.Schema{Description: field.Description, AnyOf: []*jsonschema.Schema{field, {Type: "null"}}})
	}
	metadata.Properties.Set("name", &jsonschema.Schema{Type: "string", Description: "Stable native Munki name. Defaults to the software name when creating a package."})
	metadata.Properties.Set("version", &jsonschema.Schema{Type: "string", Description: "Explicit native content version. Omit to derive a version from the inspected artifact."})
	metadata.Properties.Set("installer_type", &jsonschema.Schema{Type: "string", Enum: []any{"pkg", "copy_from_dmg", "nopkg"}, Description: "Preserve a vendor PKG, select items in an existing DMG, or publish a script-only item. PKG is inferred only from a .pkg payload."})
	return metadata
}
