// Package plugin defines versioned contracts for built-in and executable operations.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
)

// Interface versions of the operation kinds this SDK implements. A kind's
// version covers its requests and responses, the operation fields it uses and
// the framing both travel in; it changes whenever a plugin built against the
// previous version would misread a request or answer in a way the host
// misreads. Stemma uses a plugin's operations of a kind only while the plugin
// implements that kind at the host's version.
const (
	ResolveInterface   = 4
	ResourceInterface  = 4
	ReconcileInterface = 4
)

// interfaces maps each operation kind to the version this SDK implements.
var interfaces = map[string]int{"resolve": ResolveInterface, "resource": ResourceInterface, "reconcile": ReconcileInterface}

// Request invokes one advertised operation. Describe requests carry only the method.
type Request struct {
	Operation string          `json:"operation,omitempty"`
	Method    string          `json:"method"`
	Input     json.RawMessage `json:"input,omitempty"`
	LogLevel  slog.Level      `json:"log_level,omitempty"`
}

// Response retains partial Output when an operation fails, so callers can report
// the changes it completed.
type Response struct {
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Handler implements an operation and propagates cancellation through its I/O.
type Handler func(context.Context, Request) (Response, error)

// Identity identifies a logical destination independently of its display metadata.
type Identity struct {
	Project     string            `json:"project"`
	Resource    ResourceReference `json:"resource"`
	Destination string            `json:"destination"`
}

// Digest is the hex SHA-256 of the identity's project, resource key and
// destination. Destinations record it in the objects they publish, so a run
// finds them again with no state of its own.
func (i Identity) Digest() string {
	data, _ := json.Marshal([]string{i.Project, i.Resource.Key(), i.Destination})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Artifact describes immutable content. Path is present only when the engine
// leases bytes to an invocation, never a writable cache object. Version is
// selected by a consumer; Facts retain observed versions.
type Artifact struct {
	// Encoding is "tar" for a tree leased as its canonical TAR bytes.
	// Filename and Mode still describe the logical root directory.
	Encoding    string                     `json:"encoding,omitempty"`
	ContentRoot string                     `json:"content_root,omitempty"`
	Mode        uint32                     `json:"mode,omitempty"`
	EntryPoint  string                     `json:"entry_point,omitempty"`
	Evidence    map[string]json.RawMessage `json:"evidence,omitempty"`
	Path        string                     `json:"path,omitempty"`
	SHA256      string                     `json:"sha256"`
	Size        int64                      `json:"size"`
	Filename    string                     `json:"filename"`
	Format      string                     `json:"format,omitempty"`
	Tree        bool                       `json:"tree,omitempty"`
	Version     string                     `json:"version,omitempty"`
	Facts       Facts                      `json:"facts,omitzero"`
}

// ReconcileRequest carries native desired state. Raw JSON retains absent, null,
// false and empty collections; Config contains provider-owned connection settings.
// Validate requests omit Config: they check desired state without connecting,
// and plan and apply check the settings they connect with.
// Each method validates its request independently. Prepared content goes directly
// to plan or apply; validate serves declaration checks and prepare.
//
// A destination keeps no state between runs: it identifies what it manages from
// Identity and the destination itself. Peers holds declared metadata for each
// resource referenced on this connection, keyed by ResourceReference.Key().
// MinimumOS is the effective macOS requirement of the software, when known.
type ReconcileRequest[C any] struct {
	Method    string                     `json:"-"`
	Identity  Identity                   `json:"identity"`
	Config    C                          `json:"config,omitempty"`
	Metadata  json.RawMessage            `json:"metadata,omitempty"`
	Artifact  Artifact                   `json:"artifact"`
	Inputs    map[string]Artifact        `json:"inputs,omitempty"`
	MinimumOS *MinimumOS                 `json:"minimum_os,omitempty"`
	Peers     map[string]json.RawMessage `json:"peers,omitempty"`
	Prepared  bool                       `json:"prepared,omitempty"`
	Root      string                     `json:"root,omitempty"`
}

// MinimumOS is the software's declared minimum_os, or else the latest of the
// installer's requirement and the primary application's. Origin names the one
// that set Version: software.minimum_os, installer.minimum_os or app.minimum_os.
type MinimumOS struct {
	Version string `json:"version"`
	Origin  string `json:"origin"`
}

// ReconcileResponse answers each method.
//
// Content requests bytes needed to complete this method. Return it before any
// writes. The engine retries the method once with verified private copies of
// all requested artifacts; paths expire when that invocation returns. Apply
// must observe remote state again and request content if it now needs an upload.
// Apply always holds the icon input, so it is never requested.
// Consumers verify the bytes they publish against the artifact identity and
// the destination's transfer integrity contract.
//
// Each method may return Metadata in the declaration's shape, including derived
// values and nulls for cleared fields. It excludes destination-assigned values
// and preserved remote state. Origins names each managed field's source, keyed
// by the field's path in the declared metadata.
//
// Plan and apply describe the publication. Version is what clients get from
// the destination, using its native version or filename, and Previous labels
// what it held before: empty when the software is new to the
// destination, equal to Version for an update in place. Uploads are the bytes
// the method puts in the destination. Fields are the managed metadata that
// differ from the object the publication replaces: the previous version's
// object where the destination keeps one object per version, the current
// object where it updates in place, and nothing for new software, whose
// fields carry only After. Version clears remain explicit fields. Removed names
// what retention deletes, by version where the destination records one and by
// the item's own name otherwise. Destinations determine writes from native pending
// work; a failed apply's partial response reports confirmed effects.
type ReconcileResponse struct {
	Content  *ContentRequest   `json:"content,omitempty"`
	Metadata json.RawMessage   `json:"metadata,omitempty"`
	Origins  map[string]string `json:"origins,omitempty"`
	Version  string            `json:"version,omitempty"`
	Previous string            `json:"previous,omitempty"`
	Uploads  []Upload          `json:"uploads,omitempty"`
	Fields   []FieldChange     `json:"fields,omitempty"`
	// Repairs names derived destination records the method rebuilds.
	Repairs []string `json:"repairs,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// Changed reports whether the publication changes the destination.
func (r ReconcileResponse) Changed() bool {
	return r.Version != "" && r.Version != r.Previous || len(r.Uploads) > 0 || len(r.Fields) > 0 || len(r.Removed) > 0 || len(r.Repairs) > 0
}

// ContentRequest selects the primary artifact and named inputs to materialize.
type ContentRequest struct {
	Artifact bool     `json:"artifact,omitempty"`
	Inputs   []string `json:"inputs,omitempty"`
}

// Uploads a publication can make.
const (
	UploadInstaller = "installer"
	UploadIcon      = "icon"
)

// Upload identifies the payload a destination writes. Before and After are the
// destination's payload digests. Reason explains a transfer the digests alone
// cannot account for.
type Upload struct {
	Name     string `json:"name"`
	Filename string `json:"filename"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after"`
	Reason   string `json:"reason,omitempty"`
}

// FieldChange is one managed metadata field a publication sets. Field is its
// path in the declared metadata, the key Origins uses, and values name things
// as the declaration does rather than by destination IDs. Before is absent
// for a field the replaced object lacks; After is absent for a field the
// publication removes, and null clears it.
type FieldChange struct {
	Field  string          `json:"field"`
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
	ID            string          `json:"id"`
	Parent        string          `json:"parent,omitempty"`
	Kind          string          `json:"kind"`
	Path          string          `json:"path,omitempty"`
	InstalledPath string          `json:"installed_path,omitempty"`
	SHA256        string          `json:"sha256,omitempty"`
	App           *AppFacts       `json:"app,omitempty"`
	Package       *PackageFacts   `json:"package,omitempty"`
	Installer     *InstallerFacts `json:"installer,omitempty"`
	MSI           *MSIFacts       `json:"msi,omitempty"`
}

// AppFacts preserves an application's short version and build independently.
type AppFacts struct {
	BundleID   string `json:"bundle_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	Build      string `json:"build,omitempty"`
	Executable string `json:"executable,omitempty"`
	IconFile   string `json:"icon_file,omitempty"`
	IconName   string `json:"icon_name,omitempty"`
	MinimumOS  string `json:"minimum_os,omitempty"`
	// Architectures names the CPU architectures of the main executable, such
	// as arm64 and x86_64. It is absent when they are unknown: the executable
	// is missing, a link or not a Mach-O binary.
	Architectures []string `json:"architectures,omitempty"`
}

// VersionKey selects the Info.plist key holding the application's comparable
// version when none is configured. Like Munki, it prefers
// CFBundleShortVersionString unless that value does not begin with a digit and
// CFBundleVersion is available.
func (a AppFacts) VersionKey() string {
	if a.Build != "" && (a.Version == "" || a.Version[0] < '0' || a.Version[0] > '9') {
		return "CFBundleVersion"
	}
	return "CFBundleShortVersionString"
}

// PackageFacts describes a package component without asserting installer-script effects.
type PackageFacts struct {
	Identifier      string `json:"identifier,omitempty"`
	Version         string `json:"version,omitempty"`
	InstallLocation string `json:"install_location,omitempty"`
	InstalledSize   int64  `json:"installed_size,omitempty"`
	HasPayload      bool   `json:"has_payload"`
}

// InstallerFacts records an installer's static declarations without evaluating
// its scripts. Version is a Distribution's product version.
type InstallerFacts struct {
	Version       string `json:"version,omitempty"`
	MinimumOS     string `json:"minimum_os,omitempty"`
	RestartAction string `json:"restart_action,omitempty"`
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

func (request ReconcileRequest[C]) validateConfig() error {
	if request.Method == "validate" {
		return nil
	}
	return validateConfig(request.Config)
}

func (request *ReconcileRequest[C]) setMethod(method string) { request.Method = method }
