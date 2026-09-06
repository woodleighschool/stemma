package config

import (
	"encoding/json"
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/internal/intune"
	"github.com/woodleighschool/stemma/internal/jamf"
	"github.com/woodleighschool/stemma/internal/munki"
)

// Schema generates editor documentation from the same typed fields used at runtime.
// Descriptions live in Go struct tags so a released binary needs no source checkout.
func Schema() ([]byte, error) {
	r := &jsonschema.Reflector{FieldNameTag: "yaml"}
	s := r.Reflect(&Project{})
	s.ID = "https://raw.githubusercontent.com/woodleighschool/stemma/main/stemma.schema.json"
	s.Title = "Stemma project"
	s.Description = "Reproducible source recipes and native destination metadata. String values support whole-value ${VAR} environment placeholders. Omitted metadata fields remain unmanaged; null clears only supported fields."
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
	metadata := munki.MetadataSchema()
	artifactSelector := &jsonschema.Schema{Type: "string", Pattern: `^(source|prepared|[A-Za-z0-9][A-Za-z0-9._-]{0,127}/[A-Za-z0-9][A-Za-z0-9._-]{0,127})$`, Description: "Input to publish: source, prepared, artifacts/name or stepName/outputName. Omit to use the prepared source payload."}
	if verification := s.Definitions["Verification"]; verification != nil {
		if subject, ok := verification.Properties.Get("subject"); ok {
			subject.Pattern = `^(source|payload|prepared|[A-Za-z0-9][A-Za-z0-9._-]{0,127}/[A-Za-z0-9][A-Za-z0-9._-]{0,127})$`
		}
	}
	additionalInputs := &jsonschema.Schema{Type: "object", PropertyNames: &jsonschema.Schema{Pattern: namePattern.String()}, AdditionalProperties: artifactSelector, Description: "Additional named immutable inputs supplied to the destination operation, using the same artifact reference syntax."}
	metadata.Properties.Set("artifact", artifactSelector)
	metadata.Properties.Set("inputs", additionalInputs)
	// A destination's operation is declared separately. Native schemas are reusable editor
	// definitions; adapters validate the selected contract rather than a union at runtime.
	s.Definitions["MunkiMetadata"] = metadata
	s.Definitions["IntuneMetadata"] = intune.MetadataSchema()
	s.Definitions["IntuneConnection"] = intune.ConnectionSchema()
	s.Definitions["JamfMetadata"] = jamf.MetadataSchema()
	s.Definitions["JamfConnection"] = jamf.ConnectionSchema()
	s.Definitions["JamfMetadata"].Properties.Set("artifact", artifactSelector)
	s.Definitions["JamfMetadata"].Properties.Set("inputs", additionalInputs)
	for _, variant := range s.Definitions["IntuneMetadata"].OneOf {
		variant.Properties.Set("artifact", artifactSelector)
		variant.Properties.Set("inputs", additionalInputs)
		variant.Required = nil
	}
	s.Definitions["FactReference"] = &jsonschema.Schema{
		Type: "object", Required: []string{"$fact"}, AdditionalProperties: jsonschema.FalseSchema,
		Properties:  orderedmap.New[string, *jsonschema.Schema](),
		Description: "A typed observed value from exactly one named subject. Missing facts and ambiguous selectors fail before publication.",
	}
	s.Definitions["FactReference"].Properties.Set("$fact", &jsonschema.Schema{Type: "string", Pattern: `^[a-z0-9][a-z0-9_-]{0,127}\.[a-z][a-z0-9_]*(\.[A-Za-z0-9_-]+)*$`})
	for _, name := range []string{"MunkiMetadata", "IntuneMetadata", "JamfMetadata"} {
		allowFactReferences(s.Definitions[name])
	}
	if destination := s.Definitions["Destination"]; destination != nil {
		if settings, ok := destination.Properties.Get("config"); ok {
			settings.AnyOf = []*jsonschema.Schema{{Ref: "#/$defs/IntuneConnection"}, {Ref: "#/$defs/JamfConnection"}, {Type: "object", Description: "Connection configuration validated by the named operation."}}
		}
	}
	if recipe := s.Definitions["Recipe"]; recipe != nil {
		// Components may supply only part of a recipe; required source fields
		// are checked after inheritance by the runtime validator.
		recipe.Required = slices.DeleteFunc(recipe.Required, func(name string) bool { return name == "source" })
		if subjects, ok := recipe.Properties.Get("subjects"); ok {
			subjects.PropertyNames = &jsonschema.Schema{Pattern: subjectNamePattern.String()}
		}
		if destinations, ok := recipe.Properties.Get("destinations"); ok {
			destinations.Description = "Map named destinations to native writable metadata. Explicit values may reference named subject facts with {$fact: subject_name.app.version}; references retain their native type and are validated before publication."
			destinations.AdditionalProperties = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Ref: "#/$defs/MunkiMetadata"}, {Ref: "#/$defs/IntuneMetadata"}, {Ref: "#/$defs/JamfMetadata"}, {Type: "object", Description: "Native metadata and typed fact references, validated by the selected operation."}}}
		}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func allowFactReferences(schema *jsonschema.Schema) {
	if schema.Properties != nil {
		for field := schema.Properties.Oldest(); field != nil; field = field.Next() {
			if field.Key == "artifact" || field.Key == "inputs" {
				continue
			}
			allowFactReferences(field.Value)
			schema.Properties.Set(field.Key, &jsonschema.Schema{Description: field.Value.Description, AnyOf: []*jsonschema.Schema{field.Value, {Ref: "#/$defs/FactReference"}}})
		}
	}
	if schema.Items != nil {
		allowFactReferences(schema.Items)
		schema.Items = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{schema.Items, {Ref: "#/$defs/FactReference"}}}
	}
	// Referenced discriminators and derived fields cannot select one native
	// variant until resolution. Each literal branch still rejects unknown fields.
	schema.AnyOf = append(schema.AnyOf, schema.OneOf...)
	schema.OneOf = nil
	for _, variant := range schema.AnyOf {
		allowFactReferences(variant)
	}
}
