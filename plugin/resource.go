package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"go.yaml.in/yaml/v4"
)

// ResourceReference identifies a resource. An omitted API version means stemma/v1alpha1.
type ResourceReference struct {
	APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty" jsonschema:"default=stemma/v1alpha1"`
	Kind       string `json:"kind" yaml:"kind" jsonschema:"required,minLength=1"`
	Name       string `json:"name" yaml:"name" jsonschema:"required,minLength=1"`
}

// Key returns the canonical apiVersion/kind/name identity.
func (r ResourceReference) Key() string {
	version := r.APIVersion
	if version == "" {
		version = "stemma/v1alpha1"
	}
	return version + "/" + r.Kind + "/" + r.Name
}

// Validate requires an explicit kind and name; neither is inferred from a consumer.
func (r ResourceReference) Validate() error {
	if strings.TrimSpace(r.Kind) == "" || strings.TrimSpace(r.Name) == "" {
		return errors.New("resource requires kind and name")
	}
	return nil
}

// ResourceOutputReference selects an immutable output, defaulting to installer.
type ResourceOutputReference struct {
	ResourceReference `yaml:",inline"`

	Output string `json:"output,omitempty" yaml:"output,omitempty" jsonschema:"default=installer"`
}

// Input selects a resolver or a resource output. Every field beside resolver
// is the resolver's configuration, which stays opaque to the resource
// scheduler. A url or path alone selects the HTTP or filesystem resolver.
type Input struct {
	Base     string
	Resolver string
	Config   map[string]any
	Resource *ResourceOutputReference
}

func (input Input) MarshalJSON() ([]byte, error) {
	if input.Resource != nil {
		return json.Marshal(map[string]any{"resource": input.Resource})
	}
	fields := maps.Clone(input.Config)
	if fields == nil {
		fields = map[string]any{}
	}
	fields["resolver"] = input.Resolver
	return json.Marshal(fields)
}

func (input *Input) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("input must be an object")
	}
	*input = Input{}
	if value, ok := fields["resource"]; ok {
		if len(fields) != 1 {
			return errors.New("resource input cannot include resolver fields")
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input.Resource); err != nil {
			return err
		}
		if input.Resource == nil {
			return errors.New("resource requires kind and name")
		}
		return input.Resource.Validate()
	}
	if value, ok := fields["resolver"]; ok {
		if err := json.Unmarshal(value, &input.Resolver); err != nil {
			return err
		}
		if input.Resolver == "" {
			return errors.New("resolver must be a nonempty operation name")
		}
		delete(fields, "resolver")
	}
	encoded, _ := json.Marshal(fields)
	if err := json.Unmarshal(encoded, &input.Config); err != nil {
		return err
	}
	if input.Resolver == "" {
		_, hasURL := input.Config["url"]
		_, hasPath := input.Config["path"]
		switch {
		case hasURL && !hasPath:
			input.Resolver = "http"
		case hasPath && !hasURL:
			input.Resolver = "file"
		default:
			return errors.New("input needs resolver, url or path")
		}
	}
	if !ValidOperationName(input.Resolver) {
		return fmt.Errorf("invalid resolver %q", input.Resolver)
	}
	return nil
}

func (input *Input) UnmarshalYAML(node *yaml.Node) error {
	var fields map[string]any
	if err := node.Decode(&fields); err != nil {
		return err
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return input.UnmarshalJSON(data)
}

// Requirement describes a concrete runner prerequisite, independent of target OS.
type Requirement struct {
	Command   string   `json:"command"`
	Platforms []string `json:"platforms,omitempty"`
	Purpose   string   `json:"purpose"`
	Setup     string   `json:"setup"`
}

// ResourceKind associates a document schema with its preparing operation.
type ResourceKind struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ResourceRequest uses discover to return inputs and publication intentions;
// run receives only locked, leased inputs and produces immutable outputs.
type ResourceRequest[C any] struct {
	Method string `json:"-"`
	Config C      `json:"config,omitempty"`
	// Derive names a policy to observe from the inputs instead of enforcing
	// the configured value: "signature" reports the verified signer as evidence.
	Derive    string              `json:"derive,omitempty"`
	Identity  ResourceReference   `json:"identity"`
	Inputs    map[string]Artifact `json:"inputs,omitempty"`
	Workspace string              `json:"workspace,omitempty"`
	// Environment contains the immutable process values referenced by delayed
	// preparation expressions. The host includes them in preparation identity.
	Environment map[string]string `json:"environment,omitempty"`
}

// ResourceResult separates preparation configuration from destination metadata,
// so changing native publication fields cannot invalidate build outputs.
type ResourceResult struct {
	Inputs       map[string]Input          `json:"inputs,omitempty"`
	Config       json.RawMessage           `json:"config,omitempty"`
	Destinations map[string]map[string]any `json:"destinations,omitempty"`
	// Icon names the catalog asset icons/<name>.png that destinations receive
	// as the icon input. Its bytes never take part in preparation.
	Icon string `json:"icon,omitempty"`
	// MinimumOS is the declared macOS floor. Destinations receive the later of
	// it and the prepared software's own requirements, so it never takes part
	// in preparation.
	MinimumOS    string                     `json:"minimum_os,omitempty"`
	Artifacts    map[string]Artifact        `json:"artifacts,omitempty"`
	Subjects     map[string]SubjectSelector `json:"subjects,omitempty"`
	Requirements []Requirement              `json:"requirements,omitempty"`
}

// ContentContract constrains delivered content independently of its originating kind.
type ContentContract struct {
	Formats    []string `json:"formats,omitempty"`
	Trees      bool     `json:"trees,omitempty"`
	SourceFree bool     `json:"source_free,omitempty"`
}

func (contract ContentContract) Accepts(artifact Artifact) error {
	if artifact.Path == "" {
		if contract.SourceFree {
			return nil
		}
		return errors.New("destination requires an artifact")
	}
	if artifact.Tree && !contract.Trees {
		return errors.New("destination does not accept artifact trees")
	}
	if len(contract.Formats) > 0 {
		for _, format := range contract.Formats {
			if strings.EqualFold(format, artifact.Format) {
				return nil
			}
		}
		return fmt.Errorf("destination does not accept %s content", artifact.Format)
	}
	return nil
}

// ResolverKind versions an observation contract: within one version, an
// observation keeps its meaning and an immutable one always fetches the same
// bytes. Local resolvers also validate the current checkout when consuming a
// lock, rather than trusting cached bytes.
type ResolverKind struct {
	Version string `json:"version"`
	Local   bool   `json:"local,omitempty"`
}

// ResolveRequest asks discover for the declaration's current observation,
// without downloading it, and run for the artifact Observation names,
// reproducing that observation rather than looking up the latest release.
type ResolveRequest[C any] struct {
	Method      string          `json:"-"`
	Config      C               `json:"config,omitempty"`
	Base        string          `json:"base,omitempty"`
	Root        string          `json:"root,omitempty"`
	Workspace   string          `json:"workspace,omitempty"`
	Observation json.RawMessage `json:"observation,omitempty"`
}

// ResolveResponse answers discover with Observation and run with Artifact.
// Immutable promises that the observation always fetches the same bytes, so
// the host reuses content it fetched for it before instead of calling run.
type ResolveResponse struct {
	Observation json.RawMessage `json:"observation,omitempty"`
	Immutable   bool            `json:"immutable,omitempty"`
	Artifact    Artifact        `json:"artifact,omitzero"`
}

func (request *ResolveRequest[C]) setMethod(method string) { request.Method = method }

func (request ResolveRequest[C]) validateConfig() error { return validateConfig(request.Config) }

func (request *ResourceRequest[C]) setMethod(method string) { request.Method = method }

func (request ResourceRequest[C]) validateConfig() error {
	if request.Method == "discover" {
		return validateConfig(request.Config)
	}
	return nil
}
