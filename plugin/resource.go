package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// ResourceReference identifies a resource and one of its immutable outputs.
type ResourceReference struct {
	APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind       string `json:"kind" yaml:"kind"`
	Name       string `json:"name" yaml:"name"`
	Output     string `json:"output,omitempty" yaml:"output,omitempty"`
}

func (r ResourceReference) Key() string {
	version := r.APIVersion
	if version == "" {
		version = "stemma/v1alpha1"
	}
	return version + "/" + r.Kind + "/" + r.Name
}

// Input selects a resolver or a resource output. Resolver configuration stays
// opaque to the resource scheduler. URL and path declarations select the shared
// HTTP and filesystem resolvers without an extra configuration wrapper.
type Input struct {
	Base     string             `json:"-" yaml:"-"`
	Resolver string             `json:"resolver,omitempty" yaml:"resolver,omitempty"`
	Config   map[string]any     `json:"config,omitempty" yaml:"config,omitempty"`
	Resource *ResourceReference `json:"resource,omitempty" yaml:"resource,omitempty"`
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
		if input.Resource == nil || input.Resource.Kind == "" || input.Resource.Name == "" {
			return errors.New("resource requires kind and name")
		}
		return nil
	}
	if value, ok := fields["resolver"]; ok {
		if err := json.Unmarshal(value, &input.Resolver); err != nil {
			return err
		}
		delete(fields, "resolver")
	}
	if value, ok := fields["config"]; ok {
		if len(fields) != 1 {
			return errors.New("input config cannot be mixed with flat resolver fields")
		}
		if err := json.Unmarshal(value, &input.Config); err != nil {
			return err
		}
	} else {
		encoded, _ := json.Marshal(fields)
		if err := json.Unmarshal(encoded, &input.Config); err != nil {
			return err
		}
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

// ResourceRequest uses validate to discover inputs and publication intentions;
// run receives only locked, leased inputs and produces immutable outputs.
type ResourceRequest struct {
	// Cached contains leased outputs reusable during partial preparation.
	Cached map[string]Artifact `json:"cached,omitempty"`
	Config json.RawMessage     `json:"config"`
	// Derive names a policy to observe from the inputs instead of enforcing
	// the configured value: "signature" reports the verified signer as evidence.
	Derive    string              `json:"derive,omitempty"`
	Identity  ResourceReference   `json:"identity"`
	Inputs    map[string]Artifact `json:"inputs,omitempty"`
	Workspace string              `json:"workspace,omitempty"`
	Timestamp time.Time           `json:"timestamp,omitzero"`
}

// ResourceResult separates preparation configuration from destination metadata,
// so changing native publication fields cannot invalidate build outputs.
type ResourceResult struct {
	// CacheVariants separates optional outputs from the portable preparation cache.
	CacheVariants map[string]string          `json:"cache_variants,omitempty"`
	Inputs        map[string]Input           `json:"inputs,omitempty"`
	Config        json.RawMessage            `json:"config,omitempty"`
	Destinations  map[string]map[string]any  `json:"destinations,omitempty"`
	Artifacts     map[string]Artifact        `json:"artifacts,omitempty"`
	Subjects      map[string]SubjectSelector `json:"subjects,omitempty"`
	Requirements  []Requirement              `json:"requirements,omitempty"`
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

// ResolverKind versions an observation contract. Local resolvers also validate
// the current checkout when consuming a lock, rather than trusting cached bytes.
type ResolverKind struct {
	Version string `json:"version"`
	Local   bool   `json:"local,omitempty"`
}

type ResolveRequest struct {
	Config      json.RawMessage `json:"config"`
	Base        string          `json:"base,omitempty"`
	Root        string          `json:"root"`
	Workspace   string          `json:"workspace"`
	Locked      bool            `json:"locked"`
	Observation json.RawMessage `json:"observation,omitempty"`
}

type ResolveResponse struct {
	Observation json.RawMessage `json:"observation"`
	Artifact    Artifact        `json:"artifact"`
}
