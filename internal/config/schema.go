package config

import (
	"encoding/json"
	"maps"

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
	s := r.Reflect(&ProjectDocument{})
	software := r.Reflect(&SoftwareDocument{})
	maps.Copy(s.Definitions, software.Definitions)
	s.ID = "https://raw.githubusercontent.com/woodleighschool/stemma/main/stemma.schema.json"
	s.Title = "Stemma documents"
	s.Description = "One Project or Software resource per file. String values under spec support whole-value ${VAR} environment placeholders. Omitted metadata fields remain unmanaged; null clears only supported fields."
	s.Ref = ""
	s.OneOf = []*jsonschema.Schema{{Ref: "#/$defs/ProjectDocument"}, {Ref: "#/$defs/SoftwareDocument"}}
	if name, ok := s.Definitions["Metadata"].Properties.Get("name"); ok {
		name.Pattern = namePattern.String()
	}
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
	if software := s.Definitions["Software"]; software != nil {
		if source, ok := software.Properties.Get("source"); ok {
			software.Properties.Set("source", &jsonschema.Schema{Description: source.Description, AnyOf: []*jsonschema.Schema{{Ref: "#/$defs/Source"}, {Type: "null"}}})
		}
		if subjects, ok := software.Properties.Get("subjects"); ok {
			subjects.PropertyNames = &jsonschema.Schema{Pattern: subjectNamePattern.String()}
		}
		if destinations, ok := software.Properties.Get("destinations"); ok {
			destinations.Description = "Map named destinations to native writable metadata. Explicit values may reference named subject facts with {$fact: subject_name.app.version}; references retain their native type and are validated before publication."
			destinations.AdditionalProperties = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Ref: "#/$defs/MunkiMetadata"}, {Ref: "#/$defs/IntuneMetadata"}, {Ref: "#/$defs/JamfMetadata"}, {Type: "object", Description: "Native metadata and typed fact references, validated by the selected operation."}}}
		}
	}
	// Components remain partial. A document without inheritance or acquisition
	// cannot select acquired inputs; inherited requirements are checked on load.
	var acquisition jsonschema.Schema
	if err := json.Unmarshal([]byte(`{
		"if": {"anyOf": [
			{"required": ["source"], "properties": {"source": {"type": "null"}}},
			{"not": {"anyOf": [{"required": ["source"]}, {"required": ["extends"], "properties": {"extends": {"minLength": 1}}}]}}
		]},
		"then": {"properties": {
			"select": {"enum": [""]},
			"artifacts": {"maxProperties": 0},
			"steps": {"items": {"properties": {"inputs": {"additionalProperties": {"not": {"enum": ["source", "prepared"]}}}}}},
			"verification": {"properties": {"subject": {"not": {"enum": ["source", "prepared"]}}}},
			"destinations": {"additionalProperties": {"properties": {
				"artifact": {"not": {"enum": ["source", "prepared"]}},
				"inputs": {"additionalProperties": {"not": {"enum": ["source", "prepared"]}}}
			}}}
		}}
	}`), &acquisition); err != nil {
		return nil, err
	}
	s.Definitions["SoftwareDocument"].Properties.Value("spec").AllOf = []*jsonschema.Schema{&acquisition}
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
