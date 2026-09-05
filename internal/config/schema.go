package config

import (
	"encoding/json"
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/internal/intune"
	"github.com/woodleighschool/stemma/internal/jamf"
	"github.com/woodleighschool/stemma/munki"
)

// Schema generates editor documentation from the same typed fields used at runtime.
// Descriptions live in Go struct tags so a released binary needs no source checkout.
func Schema() ([]byte, error) {
	r := &jsonschema.Reflector{FieldNameTag: "yaml"}
	s := r.Reflect(&Project{})
	s.ID = "https://raw.githubusercontent.com/woodleighschool/stemma/main/stemma.schema.json"
	s.Title = "Stemma project"
	s.Description = "Reproducible source recipes and native destination metadata. Omitted metadata fields remain unmanaged; null clears only supported fields."
	project := s.Definitions["Project"]
	project.Required = slices.DeleteFunc(project.Required, func(name string) bool { return name == "recipes" })
	project.AnyOf = []*jsonschema.Schema{{Required: []string{"recipes"}}, {Required: []string{"imports"}}}
	fragment := &jsonschema.Schema{Type: "object", Properties: orderedmap.New[string, *jsonschema.Schema](), Required: []string{"version", "recipes"}, AdditionalProperties: jsonschema.FalseSchema, Description: "Software-family fragment imported by a Stemma project. Paths are relative to this file."}
	for _, name := range []string{"version", "recipes"} {
		field, _ := project.Properties.Get(name)
		fragment.Properties.Set(name, field)
	}
	s.Definitions["Fragment"] = fragment
	s.Ref = ""
	s.OneOf = []*jsonschema.Schema{{Ref: "#/$defs/Project"}, {Ref: "#/$defs/Fragment"}}
	metadataReflector := &jsonschema.Reflector{DoNotReference: true, RequiredFromJSONSchemaTags: true}
	metadata := metadataReflector.Reflect(&munki.Metadata{})
	metadata.Description = "Native Munki metadata. Omitted fields remain unmanaged; explicit lists own the whole collection."
	metadata.Properties.Set("name", &jsonschema.Schema{Type: "string", Description: "Stable native Munki name. Defaults to the recipe name when creating a package."})
	metadata.Properties.Set("version", &jsonschema.Schema{Type: "string", Description: "Explicit content version when the source or inspected installer does not provide one."})
	metadata.Properties.Set("installer_type", &jsonschema.Schema{Type: "string", Enum: []any{"pkg", "copy_from_dmg", "nopkg"}, Description: "Preserve a vendor PKG, select items in an existing DMG, or publish a script-only item. PKG is inferred only from a .pkg payload."})
	artifactSelector := &jsonschema.Schema{Type: "string", Description: "Named recipe artifact to publish. Omit to use the prepared source payload."}
	metadata.Properties.Set("artifact", artifactSelector)
	// A destination's type is declared separately. Native schemas are reusable editor
	// definitions; adapters validate the selected contract rather than a union at runtime.
	s.Definitions["MunkiMetadata"] = metadata
	s.Definitions["IntuneMetadata"] = intune.MetadataSchema()
	s.Definitions["IntuneConnection"] = intune.ConnectionSchema()
	s.Definitions["JamfMetadata"] = jamf.MetadataSchema()
	s.Definitions["JamfConnection"] = jamf.ConnectionSchema()
	s.Definitions["JamfMetadata"].Properties.Set("artifact", artifactSelector)
	for _, variant := range s.Definitions["IntuneMetadata"].OneOf {
		variant.Properties.Set("artifact", artifactSelector)
	}
	if destination := s.Definitions["Destination"]; destination != nil {
		if settings, ok := destination.Properties.Get("config"); ok {
			settings.AnyOf = []*jsonschema.Schema{{Ref: "#/$defs/IntuneConnection"}, {Ref: "#/$defs/JamfConnection"}, {Type: "object", Description: "Native plugin connection configuration."}}
		}
	}
	if recipe := s.Definitions["Recipe"]; recipe != nil {
		// Components may supply only part of a recipe; required source fields
		// are checked after inheritance by the runtime validator.
		recipe.Required = slices.DeleteFunc(recipe.Required, func(name string) bool { return name == "source" })
		if destinations, ok := recipe.Properties.Get("destinations"); ok {
			destinations.Description = "Map named destinations to their native writable metadata. Executable plugins validate their own schema."
			destinations.AdditionalProperties = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Ref: "#/$defs/MunkiMetadata"}, {Ref: "#/$defs/IntuneMetadata"}, {Ref: "#/$defs/JamfMetadata"}, {Type: "object", Description: "Native plugin metadata, validated by the selected destination."}}}
		}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
