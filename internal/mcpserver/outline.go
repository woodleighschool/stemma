package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// schema is the part of a JSON Schema an outline reads. Properties keep
// their declared order, which is the order documents use them.
type schema struct {
	Type                 types             `json:"type"`
	Title                string            `json:"title"`
	Description          string            `json:"description"`
	Enum                 []json.RawMessage `json:"enum"`
	Const                json.RawMessage   `json:"const"`
	Default              json.RawMessage   `json:"default"`
	Pattern              string            `json:"pattern"`
	Properties           properties        `json:"properties"`
	Required             []string          `json:"required"`
	Items                *schema           `json:"items"`
	AdditionalProperties *schema           `json:"additionalProperties"`
	AllOf                []*schema         `json:"allOf"`
	AnyOf                []*schema         `json:"anyOf"`
	OneOf                []*schema         `json:"oneOf"`
	// Input marks a slot that takes any input form, which describe lists as
	// resolvers rather than repeating in every kind.
	Input bool `json:"x-stemma-input"`
	// never is the false schema, such as additionalProperties: false.
	never bool
}

func (s *schema) UnmarshalJSON(data []byte) error {
	switch string(bytes.TrimSpace(data)) {
	case "true":
		*s = schema{}
		return nil
	case "false":
		*s = schema{never: true}
		return nil
	}
	type plain schema
	return json.Unmarshal(data, (*plain)(s))
}

// types accepts a type name or a list of them.
type types []string

func (t *types) UnmarshalJSON(data []byte) error {
	var name string
	if json.Unmarshal(data, &name) == nil {
		*t = types{name}
		return nil
	}
	return json.Unmarshal(data, (*[]string)(t))
}

type property struct {
	name   string
	schema *schema
}

type properties []property

func (p *properties) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("properties must be an object")
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		var value schema
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		*p = append(*p, property{token.(string), &value})
	}
	return nil
}

func parseSchema(data []byte) (*schema, error) {
	var s schema
	if len(bytes.TrimSpace(data)) == 0 {
		return &s, nil
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// member is a property of an object as documents see it: the object's own
// properties merged with those of its allOf parts and its alternatives. only
// names the alternatives that have it, when not all of them do, and values
// collects the value each alternative fixes it to.
type member struct {
	name     string
	schema   *schema
	required bool
	only     []string
	values   []string
}

// members merges an object's properties across allOf, anyOf and oneOf. A
// property of alternatives is required when every alternative requires it.
func members(s *schema) []member {
	var result []member
	index := map[string]int{}
	add := func(name string, value *schema, required bool) int {
		if i, ok := index[name]; ok {
			result[i].required = result[i].required || required
			return i
		}
		index[name] = len(result)
		result = append(result, member{name: name, schema: value, required: required})
		return len(result) - 1
	}
	var merge func(*schema)
	merge = func(s *schema) {
		for _, p := range s.Properties {
			add(p.name, p.schema, slices.Contains(s.Required, p.name))
		}
		for _, part := range s.AllOf {
			merge(part)
		}
	}
	merge(s)
	alternatives := nonNull(s)
	labels := alternativeLabels(alternatives)
	common := len(result)
	having := map[string][]string{}
	requiredIn := map[string]int{}
	variants := map[string][]*schema{}
	for i, alternative := range alternatives {
		for _, m := range members(alternative) {
			at := add(m.name, m.schema, false)
			if at < common {
				continue
			}
			if !slices.Contains(having[m.name], labels[i]) {
				having[m.name] = append(having[m.name], labels[i])
			}
			if m.required {
				requiredIn[m.name]++
			}
			variant := *m.schema
			variant.Title = labels[i]
			variants[m.name] = append(variants[m.name], &variant)
			if value := constValue(m.schema); value != "" && !slices.Contains(result[at].values, value) {
				result[at].values = append(result[at].values, value)
			}
		}
	}
	for i := common; i < len(result); i++ {
		m := &result[i]
		// Keep differing contracts as alternatives, including their nested fields.
		parts := variants[m.name]
		if len(parts) > 1 && !sameSchemas(parts) {
			m.schema = &schema{AnyOf: parts, Description: m.schema.Description}
			for _, part := range parts {
				if part.Description != m.schema.Description {
					m.schema.Description = ""
					break
				}
			}
		}
		m.required = requiredIn[m.name] == len(alternatives)
		if len(having[m.name]) < len(slices.Compact(slices.Sorted(slices.Values(labels)))) && !slices.Contains(having[m.name], "") {
			m.only = having[m.name]
		}
		if len(m.values) < 2 || slices.ContainsFunc(parts, func(part *schema) bool { return constValue(part) == "" }) {
			m.values = nil
		}
	}
	return result
}

func sameSchemas(parts []*schema) bool {
	first := *parts[0]
	first.Title = ""
	for _, part := range parts[1:] {
		other := *part
		other.Title = ""
		if !reflect.DeepEqual(first, other) {
			return false
		}
	}
	return true
}

// nonNull lists a schema's alternatives besides null.
func nonNull(s *schema) []*schema {
	var alternatives []*schema
	for _, alternative := range append(slices.Clone(s.AnyOf), s.OneOf...) {
		if !isNull(alternative) {
			alternatives = append(alternatives, alternative)
		}
	}
	return alternatives
}

// alternativeLabels names alternatives by the value of a property every one
// of them fixes, such as type=file, or else by their titles.
func alternativeLabels(alternatives []*schema) []string {
	labels := make([]string, len(alternatives))
	if len(alternatives) == 0 {
		return labels
	}
	for _, candidate := range members(alternatives[0]) {
		values := make([]string, len(alternatives))
		for i, alternative := range alternatives {
			for _, m := range members(alternative) {
				if m.name == candidate.name {
					values[i] = constValue(m.schema)
				}
			}
		}
		if !slices.Contains(values, "") && len(slices.Compact(slices.Sorted(slices.Values(values)))) == len(values) {
			for i, value := range values {
				labels[i] = candidate.name + "=" + value
			}
			return labels
		}
	}
	for i, alternative := range alternatives {
		labels[i] = alternative.Title
	}
	return labels
}

func constValue(s *schema) string {
	if len(s.Const) == 0 {
		return ""
	}
	return literal(s.Const)
}

// literal renders a JSON value compactly, with strings unquoted.
func literal(data json.RawMessage) string {
	var text string
	if json.Unmarshal(data, &text) == nil {
		return text
	}
	var compact bytes.Buffer
	if json.Compact(&compact, data) != nil {
		return string(data)
	}
	return compact.String()
}

// outline renders one line per field below path: the field's path, * when
// it is required, its type and its description.
func outline(s *schema, path string) []string {
	var lines []string
	var walk func(string, *schema)
	walk = func(prefix string, s *schema) {
		for _, m := range members(s) {
			field := join(prefix, m.name)
			lines = append(lines, line(field, m))
			if child, suffix := nested(m.schema); child != nil {
				walk(field+suffix, child)
			}
		}
	}
	walk(path, s)
	return lines
}

// nested finds the object whose fields belong below a field: its own, its
// list items' or its map values'.
func nested(s *schema) (*schema, string) {
	if len(members(s)) > 0 {
		return s, ""
	}
	if s.Items != nil && len(members(s.Items)) > 0 {
		return s.Items, "[]"
	}
	if values := s.AdditionalProperties; values != nil && !values.never && len(members(values)) > 0 {
		return values, ".<name>"
	}
	var children []*schema
	var suffix string
	for _, alternative := range nonNull(s) {
		if child, next := nested(alternative); child != nil {
			part := *child
			part.Title = alternative.Title
			children = append(children, &part)
			suffix = next
		}
	}
	if len(children) == 0 {
		return nil, ""
	}
	return &schema{AnyOf: children}, suffix
}

// field finds the member at a dotted path, passing through list items and map
// values, and returns the path it rendered as.
func field(s *schema, path string) (member, string, error) {
	current, rendered := member{schema: s}, ""
	for segment := range strings.SplitSeq(strings.ReplaceAll(path, "[]", ""), ".") {
		child, suffix := nested(current.schema)
		if child == nil {
			return member{}, "", fmt.Errorf("%s has no fields", rendered)
		}
		if suffix == ".<name>" {
			current, rendered = member{schema: child}, join(rendered, segment)
			continue
		}
		rendered += suffix
		var names []string
		found := false
		for _, m := range members(child) {
			names = append(names, m.name)
			if m.name == segment {
				current, found = m, true
			}
		}
		if !found {
			return member{}, "", fmt.Errorf("no field %s; fields are %s", join(rendered, segment), strings.Join(names, ", "))
		}
		rendered = join(rendered, segment)
	}
	return current, rendered, nil
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func line(path string, m member) string {
	text := path
	if m.required {
		text += "*"
	}
	kind := typeName(m.schema)
	if len(m.values) > 0 {
		kind = strings.Join(m.values, "|")
	}
	if len(m.only) > 0 {
		kind += "; only " + alternatives(m.only)
	}
	text += " (" + kind + ")"
	if description := strings.Join(strings.Fields(m.schema.Description), " "); description != "" {
		text += " " + description
	}
	return text
}

// alternatives joins alternative labels, naming a shared property once, as
// in type=file|registry.
func alternatives(labels []string) string {
	name, _, _ := strings.Cut(labels[0], "=")
	values := make([]string, len(labels))
	for i, label := range labels {
		prefix, value, found := strings.Cut(label, "=")
		if !found || prefix != name {
			return strings.Join(labels, "|")
		}
		values[i] = value
	}
	return name + "=" + strings.Join(values, "|")
}

func isNull(s *schema) bool {
	return len(s.Type) == 1 && s.Type[0] == "null"
}

// typeName describes a schema's values: a type with its allowed values,
// default and pattern, a list or map of them, or alternatives.
func typeName(s *schema) string {
	if s.Input {
		return "input; describe resolvers"
	}
	nullable := slices.Contains(s.Type, "null") || len(nonNull(s)) < len(s.AnyOf)+len(s.OneOf)
	var name string
	switch {
	case len(s.Const) > 0:
		name = "= " + literal(s.Const)
	case len(s.Enum) > 0:
		values := make([]string, len(s.Enum))
		for i, value := range s.Enum {
			values[i] = literal(value)
		}
		name = strings.Join(values, "|")
		if base := baseType(s); base != "" {
			name = base + ": " + name
		}
	case s.Items != nil:
		name = "list of " + typeName(s.Items)
	case len(s.Properties) > 0 || len(s.AllOf) > 0:
		name = "object"
	case s.AdditionalProperties != nil && !s.AdditionalProperties.never:
		name = "map of " + typeName(s.AdditionalProperties)
	case baseType(s) != "":
		name = baseType(s)
	case len(nonNull(s)) > 0:
		var names []string
		for _, alternative := range nonNull(s) {
			if name := typeName(alternative); !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		if len(names) > 1 {
			names = nil
			for _, alternative := range nonNull(s) {
				name := typeName(alternative)
				if alternative.Title != "" {
					name += " [" + alternative.Title + "]"
				}
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
		name = strings.Join(names, " or ")
	default:
		name = "any"
	}
	if len(s.Default) > 0 {
		name += ", default " + literal(s.Default)
	}
	if s.Pattern != "" {
		name += ", matching " + s.Pattern
	}
	if nullable {
		name += " or null"
	}
	return name
}

func baseType(s *schema) string {
	for _, name := range s.Type {
		if name != "null" {
			return name
		}
	}
	return ""
}
