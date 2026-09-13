// Package config loads project and resource documents and resolves local composition.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"

	"go.yaml.in/yaml/v4"
)

// Project is resolved configuration, rather than an authored document.
type Project struct {
	Project      string                    `json:"project"`
	Imports      []string                  `json:"imports"`
	Components   map[string]map[string]any `json:"components,omitempty"`
	Destinations map[string]Destination    `json:"destinations,omitempty"`
	Plugins      map[string]Plugin         `json:"plugins,omitempty"`
	Resources    map[string]Resource       `json:"resources"`
}

// Metadata identifies a document independently of its filename.
type Metadata struct {
	Name string `yaml:"name" json:"name" jsonschema_description:"Literal stable identity within the project, independent of the runner environment. Renaming a file does not rename its software or destination bindings."`
}

// ProjectDocument owns repository composition and destination connections.
type ProjectDocument struct {
	APIVersion string      `yaml:"apiVersion" json:"apiVersion" jsonschema:"enum=stemma/v1alpha1"`
	Kind       string      `yaml:"kind" json:"kind" jsonschema:"enum=Project"`
	Metadata   Metadata    `yaml:"metadata" json:"metadata"`
	Spec       ProjectSpec `yaml:"spec" json:"spec"`
}

// ProjectSpec contains shared settings, without inline software definitions.
type ProjectSpec struct {
	Imports      []string                  `yaml:"imports" json:"imports" jsonschema:"minItems=1" jsonschema_description:"Project-relative resource document paths or globs, such as software/**/*.yaml. Every pattern must match."`
	Components   map[string]map[string]any `yaml:"components,omitempty" json:"components,omitempty" jsonschema_description:"Reusable software defaults. Maps merge recursively; lists and null replace inherited values."`
	Destinations map[string]Destination    `yaml:"destinations,omitempty" json:"destinations,omitempty" jsonschema_description:"Named connections, separate from each resource document's native destination metadata."`
	Plugins      map[string]Plugin         `yaml:"plugins,omitempty" json:"plugins,omitempty" jsonschema_description:"Trusted local executables or OCI plugin images."`
}

// Resource is one authored contract; the registered kind owns its spec.
type Resource struct {
	APIVersion string         `yaml:"apiVersion" json:"apiVersion"`
	Kind       string         `yaml:"kind" json:"kind"`
	Metadata   Metadata       `yaml:"metadata" json:"metadata"`
	Spec       map[string]any `yaml:"spec" json:"spec"`
	Base       string         `yaml:"-" json:"-"`
}

func (r Resource) Reference() plugin.ResourceReference {
	return plugin.ResourceReference{APIVersion: r.APIVersion, Kind: r.Kind, Name: r.Metadata.Name}
}

// Destination keeps connection settings separate from native software metadata.
type Destination struct {
	Operation string         `yaml:"operation" json:"operation" jsonschema_description:"Registered destination operation, such as munki, intune or jamf. Trusted executable plugins register their own operation names."`
	Config    map[string]any `yaml:"config,omitempty" json:"config,omitempty" jsonschema_description:"Destination-specific connection configuration. Reference credential environment variables instead of embedding secrets."`
}

// Plugin selects trusted executable code independently of its distribution.
type Plugin struct {
	Trusted    bool   `yaml:"trusted" json:"trusted" jsonschema_description:"Consent to execute this plugin with the caller's privileges."`
	Image      string `yaml:"image,omitempty" json:"image,omitempty" jsonschema_description:"OCI registry reference with a tag or digest."`
	Path       string `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Local executable or directory. Relative paths resolve from the Project."`
	Entrypoint string `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty" jsonschema_description:"Executable within a local directory. Defaults to plugin, or plugin.exe on Windows."`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func parseDocument(data []byte, value any) (map[string]any, error) {
	if len(data) > 4<<20 {
		return nil, errors.New("configuration exceeds 4 MiB")
	}
	var node yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&node); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected one YAML document")
	}
	if err := checkNode(&node); err != nil {
		return nil, err
	}
	if err := decodeStrict(data, value); err != nil {
		return nil, err
	}
	var document map[string]any
	if err := node.Decode(&document); err != nil {
		return nil, err
	}
	return document, nil
}

func addResource(p *Project, resource Resource, components map[string]any, base string) error {
	if resource.Spec == nil {
		return errors.New("resource spec must be an object")
	}
	key := resource.Reference().Key()
	if _, exists := p.Resources[key]; exists {
		return fmt.Errorf("conflicting resource %s", key)
	}
	resolved, err := resolve(resource.Spec, components, nil)
	if err != nil {
		return fmt.Errorf("resource %s: %w", key, err)
	}
	resource.Spec = resolved
	resource.Base = base
	p.Resources[key] = resource
	return nil
}

func decodeStrict(data []byte, value any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(value)
}

func checkNode(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("YAML aliases are not supported; use software components")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Tag != "!!str" || key.Value == "<<" {
				return errors.New("mapping keys must be strings; YAML merge keys are not supported")
			}
			if seen[key.Value] {
				return fmt.Errorf("duplicate field %q", key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := checkNode(child); err != nil {
			return err
		}
	}
	return nil
}

func resolve(raw any, components map[string]any, stack []string) (map[string]any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("software must be a mapping")
	}
	parent, _ := m["extends"].(string)
	base := map[string]any{}
	if parent != "" {
		if slices.Contains(stack, parent) {
			return nil, fmt.Errorf("component cycle at %q", parent)
		}
		component, exists := components[parent]
		if !exists {
			return nil, fmt.Errorf("unknown component %q", parent)
		}
		var err error
		base, err = resolve(component, components, append(stack, parent))
		if err != nil {
			return nil, err
		}
	}
	merged := Merge(base, m)
	delete(merged, "extends")
	return merged, nil
}

// Merge recursively overlays declared map fields; lists and explicit null replace.
// Neither input is mutated. An empty object does not clear an inherited object.
func Merge(base, overlay map[string]any) map[string]any {
	result := make(map[string]any)
	for key, value := range base {
		if object, ok := value.(map[string]any); ok {
			result[key] = Merge(object, nil)
		} else {
			result[key] = value
		}
	}
	for key, value := range overlay {
		if object, ok := value.(map[string]any); ok {
			previous, _ := result[key].(map[string]any)
			result[key] = Merge(previous, object)
		} else {
			result[key] = value
		}
	}
	return result
}

// Validate checks references and provider-specific configuration.
func (p Project) Validate() error {
	if _, err := json.Marshal(p); err != nil {
		return fmt.Errorf("configuration must contain JSON-compatible values: %w", err)
	}
	if !namePattern.MatchString(p.Project) {
		return errors.New("project must be a stable name containing letters, digits, dots, underscores or hyphens")
	}
	if len(p.Resources) == 0 {
		return errors.New("resources must not be empty")
	}
	for _, r := range p.Resources {
		if err := validateHeader(r.APIVersion, r.Kind, "", r.Metadata); err != nil {
			return err
		}
	}
	for name, d := range p.Destinations {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid destination name %q", name)
		}
		if err := validateOperation(d.Operation); err != nil {
			return fmt.Errorf("destination %s: %w", name, err)
		}
	}
	for name, plugin := range p.Plugins {
		if !namePattern.MatchString(name) || !plugin.Trusted {
			return fmt.Errorf("plugin %s: requires a valid name and trusted: true", name)
		}
		if err := plugin.Validate(); err != nil {
			return fmt.Errorf("plugin %s: %w", name, err)
		}
	}
	return nil
}

func validateOperation(operation string) error {
	if !plugin.ValidOperationName(operation) {
		return errors.New("operation must be a named built-in or external capability")
	}
	return nil
}

func Fingerprint(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("non-JSON configuration: %v", err))
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// ValidDigest reports whether value is a canonical SHA-256 digest.
func ValidDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}
