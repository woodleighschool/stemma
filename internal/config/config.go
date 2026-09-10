// Package config loads project and software documents and resolves local composition.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/plugin"

	"go.yaml.in/yaml/v4"
	"oras.land/oras-go/v2/registry"
)

// Project is resolved configuration, rather than an authored document.
type Project struct {
	Project      string                 `json:"project"`
	Imports      []string               `json:"imports"`
	Components   map[string]Software    `json:"components,omitempty"`
	Destinations map[string]Destination `json:"destinations,omitempty"`
	Plugins      map[string]Plugin      `json:"plugins,omitempty"`
	Software     map[string]Software    `json:"software"`
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
	Imports      []string               `yaml:"imports" json:"imports" jsonschema:"minItems=1" jsonschema_description:"Project-relative Software document paths or globs, such as software/**/*.yaml. Every pattern must match."`
	Components   map[string]Software    `yaml:"components,omitempty" json:"components,omitempty" jsonschema_description:"Reusable software defaults. Maps merge recursively; lists and null replace inherited values."`
	Destinations map[string]Destination `yaml:"destinations,omitempty" json:"destinations,omitempty" jsonschema_description:"Named connections, separate from each Software document's native destination metadata."`
	Plugins      map[string]Plugin      `yaml:"plugins,omitempty" json:"plugins,omitempty" jsonschema_description:"Explicitly trusted OCI plugin images, locked by release index digest."`
}

// SoftwareDocument owns one managed item, including its acquisition and delivery.
type SoftwareDocument struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion" jsonschema:"enum=stemma/v1alpha1"`
	Kind       string   `yaml:"kind" json:"kind" jsonschema:"enum=Software"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Software `yaml:"spec" json:"spec"`
}

// Software describes acquisition and selection independently of delivery metadata.
type Software struct {
	Extends      string                     `yaml:"extends,omitempty" json:"extends,omitempty" jsonschema_description:"Local component name. Maps merge recursively; lists and null replace inherited values."`
	Source       *Source                    `yaml:"source,omitempty" json:"source,omitempty" jsonschema_description:"Optional acquisition input. Omit when operations and destinations need no acquired artifact. Its lock fingerprint excludes preparation and destination metadata."`
	Platform     string                     `yaml:"platform,omitempty" json:"platform,omitempty" jsonschema_description:"Target software platform, independent of the runner operating system."`
	Arch         string                     `yaml:"arch,omitempty" json:"arch,omitempty" jsonschema_description:"Target architecture. Universal is an explicit vendor artifact containing multiple architectures."`
	Select       string                     `yaml:"select,omitempty" json:"select,omitempty" jsonschema_description:"Exact relative payload path within an archive. Required when several plausible payloads exist."`
	Verification Verification               `yaml:"verification,omitempty" json:"verification,omitzero" jsonschema_description:"Required verification subject and scope. Unsupported required checks block publication."`
	Subjects     map[string]SubjectSelector `yaml:"subjects,omitempty" json:"subjects,omitempty" jsonschema_description:"Named selections of observed subjects for explicit fact references. Referenced selectors must match exactly one subject in the consumer input; declarations do not change delivery or detection."`
	Artifacts    map[string]Artifact        `yaml:"artifacts,omitempty" json:"artifacts,omitempty" jsonschema_description:"Named reproducible packages derived from the prepared source. Refer to a package as artifacts/name."`
	Steps        []Step                     `yaml:"steps,omitempty" json:"steps,omitempty" jsonschema_description:"Sequential operation invocations with named inputs and outputs. A step may consume only original inputs, declared artifacts or outputs of preceding steps."`
	Destinations map[string]map[string]any  `yaml:"destinations,omitempty" json:"destinations,omitempty" jsonschema_description:"Named connections. Software destination entries own only explicitly present native metadata fields."`
}

// Step invokes an operation using named artifact inputs.
type Step struct {
	Name      string            `yaml:"name" json:"name" jsonschema_description:"Unique step name used by later input references. The names source, prepared and artifacts are reserved."`
	Operation string            `yaml:"operation" json:"operation" jsonschema_description:"Registered built-in or external operation name. Its descriptor defines accepted inputs, outputs and configuration."`
	Inputs    map[string]string `yaml:"inputs,omitempty" json:"inputs,omitempty" jsonschema_description:"Map operation input names to source, prepared, artifacts/name or priorStep/outputName. Inputs may be files or trees according to the operation contract."`
	Config    map[string]any    `yaml:"config,omitempty" json:"config,omitempty" jsonschema_description:"Operation configuration validated against its registered contract."`
}

// SubjectSelector identifies one observed subject without changing its facts.
type SubjectSelector struct {
	Kind          string `yaml:"kind,omitempty" json:"kind,omitempty" jsonschema_description:"Observed subject kind. All supplied criteria must match the same subject."`
	Path          string `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Exact relative path in the inspected input, independent of the runner filesystem."`
	InstalledPath string `yaml:"installed_path,omitempty" json:"installed_path,omitempty" jsonschema_description:"Exact observed absolute installation path. This selects evidence; it does not author an installation mapping."`
	BundleID      string `yaml:"bundle_id,omitempty" json:"bundle_id,omitempty" jsonschema_description:"Exact observed application bundle identifier."`
}

// Artifact declares a portable package derived from the selected source tree.
type Artifact struct {
	Type            string            `yaml:"type" json:"type" jsonschema:"enum=pkg" jsonschema_description:"Artifact representation. Version 1 supports portable PKG creation."`
	Identifier      string            `yaml:"identifier" json:"identifier" jsonschema_description:"Stable reverse-domain package identifier."`
	Version         string            `yaml:"version" json:"version" jsonschema_description:"Explicit package version, for example 1.0."`
	Payload         string            `yaml:"payload,omitempty" json:"payload,omitempty" jsonschema_description:"Relative payload directory within the selected source tree."`
	InstallLocation string            `yaml:"install_location,omitempty" json:"install_location,omitempty" jsonschema_description:"Absolute installation destination. Defaults to /."`
	Filename        string            `yaml:"filename,omitempty" json:"filename,omitempty" jsonschema_description:"Output package basename. Defaults to the artifact name with .pkg."`
	Scripts         map[string]string `yaml:"scripts,omitempty" json:"scripts,omitempty" jsonschema_description:"Preinstall and postinstall script paths within the selected source tree. Scripts are packaged without execution."`
	From            string            `yaml:"from,omitempty" json:"from,omitempty" jsonschema:"enum=prepared" jsonschema_description:"Input artifact. Omitted or prepared selects the prepared source tree. Use a pkg step to consume other inputs."`
}

// Source specifies one acquisition provider.
type Source struct {
	Type       string   `yaml:"type" json:"type" jsonschema:"enum=http,enum=github,enum=file,enum=local" jsonschema_description:"Source provider. Unknown values are rejected."`
	Include    []string `yaml:"include,omitempty" json:"include,omitempty" jsonschema_description:"Local files and glob patterns relative to this Software document. Matched bytes, modes and symlinks determine content identity."`
	Base       string   `yaml:"-" json:"base,omitempty"`
	URL        string   `yaml:"url,omitempty" json:"url,omitempty" jsonschema_description:"Stable HTTP(S) URL without embedded credentials or expiring query parameters."`
	Match      string   `yaml:"match,omitempty" json:"match,omitempty" jsonschema_description:"HTTP download-page regular expression matching one distinct complete artifact URL. HTML entities are decoded before matching. The resolved URL is pinned in the source lock."`
	Path       string   `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Filesystem path. Source paths must stay within the Stemma project."`
	Repository string   `yaml:"repository,omitempty" json:"repository,omitempty" jsonschema_description:"GitHub repository in owner/name form."`
	Release    string   `yaml:"release,omitempty" json:"release,omitempty" jsonschema_description:"GitHub release tag or latest. New releases are resolved only during explicit updates or permitted missing lock resolution."`
	Asset      string   `yaml:"asset,omitempty" json:"asset,omitempty" jsonschema_description:"Exact GitHub release asset filename, avoiding ambiguous glob matches."`
	Filename   string   `yaml:"filename,omitempty" json:"filename,omitempty" jsonschema_description:"Retained artifact basename. Never a workspace or cache path."`
	SHA256     string   `yaml:"sha256,omitempty" json:"sha256,omitempty" jsonschema_description:"Optional independently obtained SHA-256 requirement. Explicit refresh does not bypass this requirement."`
	Token      string   `yaml:"token,omitempty" json:"token,omitempty" jsonschema_description:"Bearer token. Use ${VAR} to supply it from the environment. Excluded from source locks and fingerprints."`
}

// Verification declares the exact subject and checks required before publication.
type Verification struct {
	Subject           string `yaml:"subject,omitempty" json:"subject,omitempty" jsonschema_description:"Verify source, prepared, artifacts/name or stepName/outputName explicitly. Omitted or payload verifies each destination's primary artifact. A container signature does not verify an inner application."`
	Integrity         bool   `yaml:"integrity,omitempty" json:"integrity,omitempty" jsonschema_description:"Require all supported signed byte and hash checks for the selected subject."`
	Signature         bool   `yaml:"signature,omitempty" json:"signature,omitempty" jsonschema_description:"Require a cryptographically valid signature, separately from signer trust."`
	Resources         bool   `yaml:"resources,omitempty" json:"resources,omitempty" jsonschema_description:"Require sealed application resources. Unsupported nested-code layouts fail closed."`
	Identity          bool   `yaml:"identity,omitempty" json:"identity,omitempty" jsonschema_description:"Require authenticated signer identity using an implemented trust policy."`
	CertificateSHA256 string `yaml:"certificate_sha256,omitempty" json:"certificate_sha256,omitempty" jsonschema_description:"Exact SHA-256 pin of the authenticated PKG or supported Mach-O signer DER certificate. This does not claim CA trust or revocation assessment."`
	Platform          bool   `yaml:"platform,omitempty" json:"platform,omitempty" jsonschema_description:"Require native OS policy assessment. Unsupported on portable verifier implementations."`
}

// Destination keeps connection settings separate from native software metadata.
type Destination struct {
	Operation string         `yaml:"operation" json:"operation" jsonschema_description:"Registered destination operation, such as munki, intune or jamf. Trusted executable plugins register their own operation names."`
	Path      string         `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Local Munki repository path. Other operations use config for connection settings."`
	Config    map[string]any `yaml:"config,omitempty" json:"config,omitempty" jsonschema_description:"Destination-specific connection configuration. Reference credential environment variables instead of embedding secrets."`
}

// Plugin identifies an explicitly trusted OCI release for all runner platforms.
type Plugin struct {
	Trusted bool   `yaml:"trusted" json:"trusted" jsonschema_description:"Explicit consent to execute this plugin. Checksums prove binary identity, not publisher trust."`
	Image   string `yaml:"image" json:"image" jsonschema_description:"OCI registry reference with a tag or digest, for example ghcr.io/woodleighschool/woodstar/stemma:v1.0.0."`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var subjectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

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

func addSoftware(p *Project, name string, raw any, components map[string]any, base string) error {
	if _, exists := p.Software[name]; exists {
		return fmt.Errorf("conflicting software ID %q", name)
	}
	resolved, err := resolve(raw, components, nil)
	if err != nil {
		return fmt.Errorf("software %s: %w", name, err)
	}
	encoded, err := yaml.Marshal(resolved)
	if err != nil {
		return err
	}
	var software Software
	if err := decodeStrict(encoded, &software); err != nil {
		return err
	}
	if software.Source != nil && software.Source.Type == "file" {
		filename := software.Source.Path
		if filename == "" || strings.HasPrefix(filename, "/") || strings.ContainsAny(filename, "\\:\x00\r\n") {
			return fmt.Errorf("software %s: file source path must be relative", name)
		}
		software.Source.Path = path.Join(base, filename)
		if !safeRelative(software.Source.Path) {
			return fmt.Errorf("software %s: file source path must remain within the project", name)
		}
	}
	if software.Source != nil && software.Source.Type == "local" {
		software.Source.Base = base
	}
	p.Software[name] = software
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
	result := make(map[string]any, len(base)+len(overlay))
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
	if len(p.Software) == 0 {
		return errors.New("software must not be empty")
	}
	for name, r := range p.Software {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid software name %q", name)
		}
		if r.Source != nil {
			if err := r.Source.Validate(); err != nil {
				return fmt.Errorf("software %s: %w", name, err)
			}
		} else if r.Select != "" || len(r.Artifacts) != 0 {
			return fmt.Errorf("software %s: select and derived artifacts require a source", name)
		}
		if r.Platform != "" && r.Platform != "darwin" && r.Platform != "linux" && r.Platform != "windows" {
			return fmt.Errorf("software %s: unsupported platform", name)
		}
		if r.Arch != "" && r.Arch != "amd64" && r.Arch != "arm64" && r.Arch != "universal" {
			return fmt.Errorf("software %s: unsupported architecture", name)
		}
		if r.Select != "" && !filepath.IsLocal(filepath.FromSlash(r.Select)) {
			return fmt.Errorf("software %s: select must be a relative path", name)
		}
		if r.Verification.CertificateSHA256 != "" && !ValidDigest(r.Verification.CertificateSHA256) {
			return fmt.Errorf("software %s: invalid certificate SHA-256", name)
		}
		for artifact, value := range r.Artifacts {
			if !namePattern.MatchString(artifact) {
				return fmt.Errorf("software %s: invalid artifact name %q", name, artifact)
			}
			if err := value.Validate(); err != nil {
				return fmt.Errorf("software %s artifact %s: %w", name, artifact, err)
			}
		}
		for subject, selector := range r.Subjects {
			if !subjectNamePattern.MatchString(subject) {
				return fmt.Errorf("software %s: subject name %q must contain lowercase letters, digits, underscores or hyphens", name, subject)
			}
			if err := selector.Validate(); err != nil {
				return fmt.Errorf("software %s subject %s: %w", name, subject, err)
			}
		}
		steps := make(map[string]bool, len(r.Steps))
		for _, step := range r.Steps {
			if !namePattern.MatchString(step.Name) || step.Name == "source" || step.Name == "prepared" || step.Name == "artifacts" {
				return fmt.Errorf("software %s: invalid or reserved step name %q", name, step.Name)
			}
			if steps[step.Name] {
				return fmt.Errorf("software %s: duplicate step name %q", name, step.Name)
			}
			if err := validateOperation(step.Operation); err != nil {
				return fmt.Errorf("software %s step %s: %w", name, step.Name, err)
			}
			for input, reference := range step.Inputs {
				if !namePattern.MatchString(input) {
					return fmt.Errorf("software %s step %s: invalid input name %q", name, step.Name, input)
				}
				if err := validateArtifactReference(reference, r.Source != nil, r.Artifacts, steps); err != nil {
					return fmt.Errorf("software %s step %s input %s: %w", name, step.Name, input, err)
				}
			}
			steps[step.Name] = true
		}
		if subject := r.Verification.Subject; subject != "" && subject != "payload" {
			if err := validateArtifactReference(subject, r.Source != nil, r.Artifacts, steps); err != nil {
				return fmt.Errorf("software %s verification subject: %w", name, err)
			}
		}
		for destination, metadata := range r.Destinations {
			if _, exists := p.Destinations[destination]; !exists {
				return fmt.Errorf("software %s: unknown destination %q", name, destination)
			}
			if selected, exists := metadata["artifact"]; exists {
				artifact, ok := selected.(string)
				if !ok {
					return fmt.Errorf("software %s destination %s: artifact must be an input reference string", name, destination)
				}
				if err := validateArtifactReference(artifact, r.Source != nil, r.Artifacts, steps); err != nil {
					return fmt.Errorf("software %s destination %s: %w", name, destination, err)
				}
			}
			if value, exists := metadata["inputs"]; exists {
				inputs, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("software %s destination %s: inputs must map input names to artifact reference strings", name, destination)
				}
				for input, value := range inputs {
					reference, ok := value.(string)
					if !namePattern.MatchString(input) || !ok {
						return fmt.Errorf("software %s destination %s: inputs require safe names and artifact reference strings", name, destination)
					}
					if err := validateArtifactReference(reference, r.Source != nil, r.Artifacts, steps); err != nil {
						return fmt.Errorf("software %s destination %s input %s: %w", name, destination, input, err)
					}
				}
			}
		}
	}
	for name, d := range p.Destinations {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid destination name %q", name)
		}
		if err := validateOperation(d.Operation); err != nil {
			return fmt.Errorf("destination %s: %w", name, err)
		}
		if d.Operation == "munki" {
			if d.Path == "" || len(d.Config) != 0 {
				return fmt.Errorf("destination %s: munki requires only path", name)
			}
		} else if d.Path != "" {
			return fmt.Errorf("destination %s: use config for connection settings", name)
		}
	}
	for name, plugin := range p.Plugins {
		if !namePattern.MatchString(name) || !plugin.Trusted {
			return fmt.Errorf("plugin %s: requires a valid name and trusted: true", name)
		}
		ref, err := registry.ParseReference(plugin.Image)
		if err != nil || ref.Reference == "" {
			return fmt.Errorf("plugin %s: image must be an OCI registry reference with a tag or digest", name)
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

func validateArtifactReference(reference string, source bool, artifacts map[string]Artifact, steps map[string]bool) error {
	if reference == "source" || reference == "prepared" {
		if !source {
			return fmt.Errorf("input reference %q requires a source", reference)
		}
		return nil
	}
	owner, output, ok := strings.Cut(reference, "/")
	if !ok || !namePattern.MatchString(owner) || !namePattern.MatchString(output) {
		return errors.New("input reference must be source, prepared, artifacts/name or stepName/outputName")
	}
	if owner == "artifacts" {
		if _, exists := artifacts[output]; !exists {
			return fmt.Errorf("unknown artifact %q", output)
		}
		return nil
	}
	if !steps[owner] {
		return fmt.Errorf("input reference %q must name an existing preceding step", reference)
	}
	return nil
}

// Validate checks selector criteria without consulting the runner filesystem.
func (s SubjectSelector) Validate() error {
	if s.Kind == "" && s.Path == "" && s.InstalledPath == "" && s.BundleID == "" {
		return errors.New("subject selector requires at least one criterion")
	}
	if s.Kind != "" && !namePattern.MatchString(s.Kind) {
		return errors.New("subject kind must be a safe name")
	}
	if s.Path != "" && (!safeRelative(s.Path) || path.Clean(s.Path) != s.Path) {
		return errors.New("subject path must be a clean relative POSIX path")
	}
	if s.InstalledPath != "" && (!path.IsAbs(s.InstalledPath) || path.Clean(s.InstalledPath) != s.InstalledPath || strings.ContainsAny(s.InstalledPath, "\\\x00\r\n\t")) {
		return errors.New("subject installed_path must be a clean absolute POSIX path")
	}
	if !utf8.ValidString(s.BundleID) || strings.ContainsAny(s.BundleID, "\x00\r\n\t") || (s.BundleID != "" && strings.TrimSpace(s.BundleID) != s.BundleID) {
		return errors.New("subject bundle_id must be a nonempty single-line identifier")
	}
	return nil
}

// Validate checks exactly one source provider and prevents credentials in URLs.
func (s Source) Validate() error {
	if s.Filename != "" && (filepath.Base(s.Filename) != s.Filename || strings.ContainsAny(s.Filename, `/\\`) || s.Filename == "." || s.Filename == "..") {
		return errors.New("filename must be a basename")
	}
	if s.SHA256 != "" && !ValidDigest(s.SHA256) {
		return errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	if s.Type != "local" && (len(s.Include) > 0 || s.Base != "") {
		return errors.New("include is only supported for local sources")
	}
	if s.Match != "" {
		if s.Type != "http" {
			return errors.New("match is only supported for HTTP sources")
		}
		if _, err := regexp.Compile(s.Match); err != nil {
			return fmt.Errorf("source match: %w", err)
		}
	}
	switch s.Type {
	case "http":
		if err := ValidateHTTPURL(s.URL); err != nil {
			return err
		}
		if s.Path != "" || s.Repository != "" || s.Release != "" || s.Asset != "" {
			return errors.New("HTTP source contains fields for another provider")
		}
	case "github":
		if len(strings.Split(s.Repository, "/")) != 2 || strings.ContainsAny(s.Repository, " ?#\\") || s.Asset == "" || s.URL != "" || s.Path != "" {
			return errors.New("GitHub source requires repository owner/name and an exact asset name")
		}
	case "file":
		if !safeRelative(s.Path) || s.URL != "" || s.Repository != "" || s.Release != "" || s.Asset != "" || s.Token != "" {
			return errors.New("file source requires a project-relative path only")
		}
	case "local":
		if len(s.Include) == 0 || s.Path != "" || s.URL != "" || s.Repository != "" || s.Release != "" || s.Asset != "" || s.Token != "" || (s.Base != "" && !safeRelative(s.Base)) {
			return errors.New("local source requires include patterns relative to its software-family file")
		}
		for _, pattern := range s.Include {
			if !safeRelative(pattern) || !doublestar.ValidatePattern(pattern) {
				return fmt.Errorf("invalid local include pattern %q", pattern)
			}
		}
	default:
		return fmt.Errorf("unsupported source type %q", s.Type)
	}
	return nil
}

// Validate checks the supported package derivation and confined source paths.
func (a Artifact) Validate() error {
	if a.Type != "pkg" || (a.From != "" && a.From != "prepared") {
		return errors.New("artifact type must be pkg and from must be prepared or omitted")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`).MatchString(a.Identifier) || len(a.Identifier) > 255 {
		return errors.New("package identifier must be a nonempty reverse-domain identifier")
	}
	if a.Version == "" || len(a.Version) > 128 || !utf8.ValidString(a.Version) || strings.ContainsAny(a.Version, "\x00\r\n\t") {
		return errors.New("package version must be a nonempty single-line string")
	}
	if a.Payload == "" && len(a.Scripts) == 0 {
		return errors.New("package requires payload or install scripts")
	}
	if a.Payload != "" && (!safeRelative(a.Payload) || path.Clean(a.Payload) != a.Payload) {
		return errors.New("package payload must be a confined relative path")
	}
	if a.InstallLocation != "" && (!path.IsAbs(a.InstallLocation) || path.Clean(a.InstallLocation) != a.InstallLocation || strings.ContainsAny(a.InstallLocation, "\\\x00\r\n\t")) {
		return errors.New("package install_location must be a clean absolute POSIX path")
	}
	if a.Filename != "" && (!safeRelative(a.Filename) || path.Base(a.Filename) != a.Filename || !strings.HasSuffix(strings.ToLower(a.Filename), ".pkg")) {
		return errors.New("package filename must be a .pkg basename")
	}
	for name, filename := range a.Scripts {
		if name != "preinstall" && name != "postinstall" {
			return fmt.Errorf("unsupported package script %q", name)
		}
		if !safeRelative(filename) || path.Clean(filename) != filename || filename == "." {
			return fmt.Errorf("package script %s must be a confined relative file path", name)
		}
	}
	return nil
}

// ValidateHTTPURL allows stable query identifiers, but excludes embedded
// credentials and commonly signed, expiring download references from locks.
func ValidateHTTPURL(address string) error {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("HTTP source requires an http(s) URL without credentials or fragment")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("HTTP source contains an invalid query")
	}
	for key := range query {
		key = strings.ToLower(key)
		if strings.HasPrefix(key, "x-amz-") || strings.HasPrefix(key, "x-goog-") {
			return errors.New("HTTP source must use a stable URL, not an expiring signed download")
		}
		switch strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", "") {
		case "token", "accesstoken", "authtoken", "auth", "authorization", "apikey", "key", "signature", "sig", "expires", "expiry", "expiration", "credential", "credentials", "password", "secret":
			return errors.New("HTTP source query must not contain credentials or expiration")
		}
	}
	return nil
}

// Fingerprint identifies acquisition settings independently of credentials.
func (s Source) Fingerprint() string {
	s.Token = ""
	return Fingerprint(s)
}

// Fingerprint identifies a destination without its authentication secrets.
func (d Destination) Fingerprint() string {
	if d.Operation == "intune" || d.Operation == "jamf" {
		d.Config = maps.Clone(d.Config)
		delete(d.Config, "token")
		delete(d.Config, "client_secret")
	}
	return Fingerprint(d)
}

// Fingerprint returns a canonical digest, independent of map iteration order.
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
