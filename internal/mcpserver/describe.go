package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

type describeInput struct {
	Kind        string `json:"kind,omitempty" jsonschema:"Resource kind, such as MacSoftware."`
	Resolver    string `json:"resolver,omitempty" jsonschema:"Input resolver, such as http or github."`
	Destination string `json:"destination,omitempty" jsonschema:"Project destination name, as a resource's destinations key it."`
	Field       string `json:"field,omitempty" jsonschema:"Dotted path of one nested block to describe, such as pkginfo or targets.include."`
}

// description answers describe: what the project offers or, for a selected
// kind, resolver or destination, that entry and its fields.
type description struct {
	Project      string               `json:"project,omitempty"`
	Kinds        []kindSummary        `json:"kinds,omitempty"`
	Resolvers    []resolverSummary    `json:"resolvers,omitempty"`
	Destinations []destinationSummary `json:"destinations,omitempty"`
	Components   map[string]any       `json:"components,omitempty"`
	Unavailable  []string             `json:"unavailable,omitempty"`
	Fields       []string             `json:"fields,omitempty"`
}

type kindSummary struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"api_version,omitempty"`
}

type resolverSummary struct {
	Name   string              `json:"name"`
	Fields []string            `json:"fields,omitempty"`
	Values map[string][]string `json:"values,omitempty"`
}

type destinationSummary struct {
	Name       string   `json:"name"`
	Operation  string   `json:"operation"`
	Formats    []string `json:"formats,omitempty"`
	SourceFree bool     `json:"source_free,omitempty"`
}

const defaultAPIVersion = "stemma/v1alpha1"

func (c catalog) describe(ctx context.Context, _ *mcp.CallToolRequest, in describeInput) (*mcp.CallToolResult, description, error) {
	named := 0
	for _, name := range []string{in.Kind, in.Resolver, in.Destination} {
		if name != "" {
			named++
		}
	}
	switch {
	case named > 1:
		return nil, description{}, errors.New("name one of kind, resolver or destination")
	case in.Field != "" && named == 0:
		return nil, description{}, errors.New("field narrows a kind, resolver or destination; name one")
	}
	contracts, err := engine.ProjectContracts(ctx, c.options(""))
	if err != nil {
		return nil, description{}, err
	}
	result := description{Unavailable: contracts.Unavailable}
	var contract json.RawMessage
	switch {
	case in.Kind != "":
		operation, err := kind(contracts, in.Kind)
		if err != nil {
			return nil, description{}, err
		}
		result.Kinds = []kindSummary{summarizeKind(operation)}
		result.Destinations = destinations(contracts)
		contract = operation.ConfigSchema
	case in.Resolver != "":
		if contract, err = resolverSchema(contracts, in.Resolver); err != nil {
			return nil, description{}, err
		}
		result.Resolvers = []resolverSummary{{Name: in.Resolver}}
	case in.Destination != "":
		summary, operation, err := destination(contracts, in.Destination)
		if err != nil {
			return nil, description{}, err
		}
		result.Destinations = []destinationSummary{summary}
		contract = operation.MetadataSchema
	default:
		result.Project = contracts.Project
		for _, operation := range contracts.Operations {
			if operation.Resource != nil {
				result.Kinds = append(result.Kinds, summarizeKind(operation))
			}
		}
		if result.Resolvers, err = resolvers(contracts); err != nil {
			return nil, description{}, err
		}
		result.Destinations = destinations(contracts)
		if len(contracts.Components) > 0 {
			result.Components = map[string]any{}
			for name, component := range contracts.Components {
				result.Components[name] = component
			}
		}
		return nil, result, nil
	}
	result.Fields, err = fields(contract, in.Field)
	return nil, result, err
}

func (c catalog) options(method string) engine.Options {
	return engine.Options{ConfigPath: c.config, CacheDir: c.cache, Method: method}
}

func summarizeKind(operation plugin.Operation) kindSummary {
	summary := kindSummary{Kind: operation.Resource.Kind}
	if operation.Resource.APIVersion != defaultAPIVersion {
		summary.APIVersion = operation.Resource.APIVersion
	}
	return summary
}

// kind finds a resource kind by name, or by apiVersion/Kind when several API
// versions share it.
func kind(contracts engine.Contracts, name string) (plugin.Operation, error) {
	var names []string
	var matches []plugin.Operation
	for _, operation := range contracts.Operations {
		if operation.Resource == nil {
			continue
		}
		kind := *operation.Resource
		if name == kind.APIVersion+"/"+kind.Kind {
			return operation, nil
		}
		if name == kind.Kind {
			matches = append(matches, operation)
		}
		if !slices.Contains(names, kind.Kind) {
			names = append(names, kind.Kind)
		}
	}
	switch len(matches) {
	case 0:
		return plugin.Operation{}, fmt.Errorf("unknown kind %s; kinds are %s", name, strings.Join(names, ", "))
	case 1:
		return matches[0], nil
	}
	var choices []string
	for _, operation := range matches {
		choices = append(choices, operation.Resource.APIVersion+"/"+operation.Resource.Kind)
	}
	return plugin.Operation{}, fmt.Errorf("kind %s is ambiguous; use %s", name, strings.Join(choices, " or "))
}

func resolverSchema(contracts engine.Contracts, name string) (json.RawMessage, error) {
	if schema := source.ResolverSchema(name); schema != nil {
		return json.Marshal(schema)
	}
	names := source.Resolvers()
	for _, operation := range contracts.Operations {
		if operation.Resolver == nil {
			continue
		}
		if operation.Name == name {
			return operation.ConfigSchema, nil
		}
		names = append(names, operation.Name)
	}
	return nil, fmt.Errorf("unknown resolver %s; resolvers are %s", name, strings.Join(names, ", "))
}

// resolvers lists the built-in resolvers, then the plugins', with their fields
// and the values those fields accept.
func resolvers(contracts engine.Contracts) ([]resolverSummary, error) {
	names := source.Resolvers()
	for _, operation := range contracts.Operations {
		if operation.Resolver != nil {
			names = append(names, operation.Name)
		}
	}
	var summaries []resolverSummary
	for _, name := range names {
		data, err := resolverSchema(contracts, name)
		if err != nil {
			return nil, err
		}
		root, err := parseSchema(data)
		if err != nil {
			return nil, fmt.Errorf("resolver %s schema: %w", name, err)
		}
		summary := resolverSummary{Name: name}
		for _, m := range members(root) {
			label := m.name
			if m.required {
				label += "*"
			}
			summary.Fields = append(summary.Fields, label)
			if values := enumValues(m.schema); len(values) > 0 {
				if summary.Values == nil {
					summary.Values = map[string][]string{}
				}
				summary.Values[m.name] = values
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func enumValues(s *schema) []string {
	values := make([]string, len(s.Enum))
	for i, value := range s.Enum {
		values[i] = literal(value)
	}
	return values
}

func destinations(contracts engine.Contracts) []destinationSummary {
	var summaries []destinationSummary
	for _, name := range slices.Sorted(maps.Keys(contracts.Destinations)) {
		summary, _, _ := destination(contracts, name)
		summaries = append(summaries, summary)
	}
	return summaries
}

// destination summarizes a destination connection and finds its operation,
// which fails when the plugin providing it did not load.
func destination(contracts engine.Contracts, name string) (destinationSummary, plugin.Operation, error) {
	operationName, ok := contracts.Destinations[name]
	if !ok {
		return destinationSummary{}, plugin.Operation{}, fmt.Errorf("unknown destination %s; destinations are %s", name, strings.Join(slices.Sorted(maps.Keys(contracts.Destinations)), ", "))
	}
	summary := destinationSummary{Name: name, Operation: operationName}
	for _, operation := range contracts.Operations {
		if operation.Name != operationName {
			continue
		}
		if content := operation.Content; content != nil {
			summary.Formats, summary.SourceFree = content.Formats, content.SourceFree
		}
		return summary, operation, nil
	}
	return summary, plugin.Operation{}, fmt.Errorf("destination %s: operation %s is unavailable", name, operationName)
}

// fields outlines a contract, or the one nested block path names.
func fields(contract json.RawMessage, path string) ([]string, error) {
	root, err := parseSchema(contract)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return outline(root, ""), nil
	}
	target, rendered, err := field(root, path)
	if err != nil {
		return nil, err
	}
	lines := []string{line(rendered, target)}
	if child, suffix := nested(target.schema); child != nil {
		lines = append(lines, outline(child, rendered+suffix)...)
	}
	return lines, nil
}
