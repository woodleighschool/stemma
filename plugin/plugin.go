// Package plugin defines versioned contracts for built-in and executable operations.
package plugin

import (
	"context"
	"encoding/json"
	"time"
)

// ProtocolVersion is the executable protocol understood by this SDK.
const ProtocolVersion = 2

// Request invokes one advertised operation. Describe requests omit Operation and Input.
type Request struct {
	Protocol  int             `json:"protocol"`
	Operation string          `json:"operation,omitempty"`
	Method    string          `json:"method"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// Response retains partial Output when an operation fails. Callers must persist
// recovered reconciliation bindings even when Error is present.
type Response struct {
	Protocol int             `json:"protocol"`
	Output   json.RawMessage `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Handler implements an operation and propagates cancellation through its I/O.
type Handler func(context.Context, Request) (Response, error)

// Identity identifies a logical destination independently of its display metadata.
type Identity struct {
	Project     string `json:"project"`
	Software    string `json:"software"`
	Destination string `json:"destination"`
}

// Artifact is an immutable file or tree leased by the engine, never a writable
// cache object. Version is selected by a consumer; Facts retain observed versions.
type Artifact struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Filename string `json:"filename"`
	Format   string `json:"format,omitempty"`
	Tree     bool   `json:"tree,omitempty"`
	Version  string `json:"version,omitempty"`
	Facts    Facts  `json:"facts,omitzero"`
}

// StepRequest supplies leased inputs and a writable workspace. Timestamp is the
// retained source timestamp, allowing package creation to remain deterministic.
type StepRequest struct {
	Config    json.RawMessage     `json:"config,omitempty"`
	Inputs    map[string]Artifact `json:"inputs,omitempty"`
	Workspace string              `json:"workspace"`
	Timestamp time.Time           `json:"timestamp,omitzero"`
}

// StepResponse names produced artifacts and preserves their observed facts.
type StepResponse struct {
	Artifacts map[string]Artifact `json:"artifacts,omitempty"`
	Facts     Facts               `json:"facts,omitzero"`
}

// ReconcileRequest carries native desired state. Raw JSON retains absent, null,
// false and empty collections; Config contains provider-owned connection settings.
type ReconcileRequest struct {
	Method   string                     `json:"method"`
	Identity Identity                   `json:"identity"`
	Config   json.RawMessage            `json:"config,omitempty"`
	Metadata json.RawMessage            `json:"metadata,omitempty"`
	Binding  json.RawMessage            `json:"binding,omitempty"`
	Artifact Artifact                   `json:"artifact"`
	Inputs   map[string]Artifact        `json:"inputs,omitempty"`
	Facts    Facts                      `json:"facts,omitzero"`
	Subjects map[string]SubjectSelector `json:"subjects,omitempty"`
	Bindings map[string]json.RawMessage `json:"bindings,omitempty"`
	Prepared bool                       `json:"prepared,omitempty"`
	Root     string                     `json:"root,omitempty"`
}

// ReconcileResponse carries changes and recovered durable bindings. An omitted
// Binding preserves it, null clears it, and a value replaces it, including on error.
type ReconcileResponse struct {
	Changes  []Change          `json:"changes,omitempty"`
	Binding  json.RawMessage   `json:"binding,omitempty"`
	Origins  map[string]string `json:"origins,omitempty"`
	Requires []string          `json:"requires,omitempty"`
}

// Change is a semantic destination change; an empty Changes list needs no write.
type Change struct {
	Kind   string          `json:"kind"`
	Field  string          `json:"field"`
	Action string          `json:"action"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

// FactsVersion identifies the model used for observed artifact evidence.
const FactsVersion = 1

// Facts retains separately identified subjects instead of collapsing their versions.
type Facts struct {
	Version  int       `json:"version"`
	Subjects []Subject `json:"subjects,omitempty"`
}

// Subject records containment and path provenance. Path belongs to the inspected
// artifact; InstalledPath is an installation location, never a runner lookup path.
type Subject struct {
	ID            string        `json:"id"`
	Parent        string        `json:"parent,omitempty"`
	Kind          string        `json:"kind"`
	Path          string        `json:"path,omitempty"`
	InstalledPath string        `json:"installed_path,omitempty"`
	SHA256        string        `json:"sha256,omitempty"`
	App           *AppFacts     `json:"app,omitempty"`
	Package       *PackageFacts `json:"package,omitempty"`
	MSI           *MSIFacts     `json:"msi,omitempty"`
}

// AppFacts preserves an application's short version and build independently.
type AppFacts struct {
	BundleID   string `json:"bundle_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	Build      string `json:"build,omitempty"`
	Executable string `json:"executable,omitempty"`
	MinimumOS  string `json:"minimum_os,omitempty"`
}

// PackageFacts describes a package component without asserting installer-script effects.
type PackageFacts struct {
	Identifier      string `json:"identifier,omitempty"`
	Version         string `json:"version,omitempty"`
	InstallLocation string `json:"install_location,omitempty"`
	InstalledSize   int64  `json:"installed_size,omitempty"`
	HasPayload      bool   `json:"has_payload"`
}

// MSIFacts preserves MSI database identity and its native property names.
type MSIFacts struct {
	ProductCode    string            `json:"product_code,omitempty"`
	ProductVersion string            `json:"product_version,omitempty"`
	ProductName    string            `json:"product_name,omitempty"`
	Manufacturer   string            `json:"manufacturer,omitempty"`
	UpgradeCode    string            `json:"upgrade_code,omitempty"`
	PackageCode    string            `json:"package_code,omitempty"`
	Properties     map[string]string `json:"properties,omitempty"`
}
