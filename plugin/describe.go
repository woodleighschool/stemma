package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Description is a plugin's answer to describe as this SDK reads it. The
// embedded Descriptor holds the plugin's identity and the operations this SDK
// can use; Unavailable holds the ones whose kind the plugin implements at
// another interface version.
type Description struct {
	Descriptor

	Unavailable []Unavailable
}

// Unavailable names an operation this SDK cannot use and why. Only the
// operation's identity is read, so a host can report it where it is used.
type Unavailable struct {
	Name     string        `json:"name"`
	Kind     string        `json:"kind"`
	Resource *ResourceKind `json:"resource,omitempty"`
	Reason   string        `json:"reason"`
}

// Describe runs an executable's describe method and reads its descriptor:
// first the identity every interface version shares, then, strictly, the
// operations of each kind the plugin implements at this SDK's version. Other
// operations are returned as unavailable rather than failing the plugin.
func Describe(ctx context.Context, executable string) (Description, error) {
	response, err := Run(ctx, executable, Request{Method: "describe"})
	if err != nil {
		return Description{}, fmt.Errorf("describe: %w", err)
	}
	var header struct {
		Name       string            `json:"name"`
		Version    string            `json:"version"`
		Revision   string            `json:"revision"`
		Interfaces map[string]int    `json:"interfaces"`
		Operations []json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(response.Output, &header); err != nil {
		return Description{}, fmt.Errorf("descriptor: %w", err)
	}
	description := Description{Name: header.Name, Version: header.Version, Revision: header.Revision, Interfaces: header.Interfaces, Operations: []Operation{}}
	seen := make(map[string]bool, len(header.Operations))
	for _, data := range header.Operations {
		var identity struct {
			Name     string        `json:"name"`
			Kind     string        `json:"kind"`
			Resource *ResourceKind `json:"resource"`
		}
		if err := json.Unmarshal(data, &identity); err != nil {
			return description, fmt.Errorf("descriptor: %w", err)
		}
		if seen[identity.Name] {
			return description, fmt.Errorf("descriptor: operation %q is advertised more than once", identity.Name)
		}
		seen[identity.Name] = true
		if reason := compatibility(identity.Kind, header.Interfaces[identity.Kind]); reason != "" {
			description.Unavailable = append(description.Unavailable, Unavailable{Name: identity.Name, Kind: identity.Kind, Resource: identity.Resource, Reason: reason})
			continue
		}
		var operation Operation
		if err := decode(bytes.NewReader(data), &operation); err != nil {
			return description, fmt.Errorf("descriptor operation %q: %w", identity.Name, err)
		}
		description.Operations = append(description.Operations, operation)
	}
	if err := ValidateDescriptor(description.Descriptor); err != nil {
		return description, fmt.Errorf("descriptor: %w", err)
	}
	return description, nil
}

// compatibility explains why this SDK cannot use a kind a plugin implements at
// version, or returns "" when it can.
func compatibility(kind string, version int) string {
	current, known := interfaces[kind]
	switch {
	case !known:
		return fmt.Sprintf("operation kind %q is unknown to this Stemma", kind)
	case version == 0:
		return fmt.Sprintf("names no %s interface version", kind)
	case version != current:
		return fmt.Sprintf("implements %s interface %d; this Stemma uses %d", kind, version, current)
	}
	return ""
}
