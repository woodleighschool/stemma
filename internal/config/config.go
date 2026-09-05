// Package config loads strict project configuration and resolves local recipe composition.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"

	"go.yaml.in/yaml/v4"
)

// Project is the resolved configuration of a Stemma root.
type Project struct {
	Version      int                    `yaml:"version" json:"version" jsonschema:"enum=1" jsonschema_description:"Configuration format version. Use 1."`
	Project      string                 `yaml:"project" json:"project" jsonschema_description:"Stable project identity used to recover destination bindings. Keep it unchanged when cloning or moving the repository."`
	Imports      []string               `yaml:"imports,omitempty" json:"imports,omitempty" jsonschema_description:"Project-relative software-family files, for example software/**/stemma.yaml. Every pattern must match."`
	Components   map[string]Recipe      `yaml:"components,omitempty" json:"components,omitempty" jsonschema_description:"Reusable local recipe components. Resolved inherited fields retain their metadata ownership."`
	Recipes      map[string]Recipe      `yaml:"recipes" json:"recipes" jsonschema_description:"Named software recipes. Use separate names for platform and architecture variants."`
	Destinations map[string]Destination `yaml:"destinations,omitempty" json:"destinations,omitempty" jsonschema_description:"Named connections. Recipe destination entries own only explicitly present native metadata fields."`
	Plugins      map[string]Plugin      `yaml:"plugins,omitempty" json:"plugins,omitempty" jsonschema_description:"Explicitly trusted executable extensions with independently locked platform binaries."`
}

// Recipe describes acquisition and selection independently of delivery metadata.
type Recipe struct {
	Extends      string                    `yaml:"extends,omitempty" json:"extends,omitempty" jsonschema_description:"Local component name. Maps merge recursively; lists and null replace inherited values."`
	Source       Source                    `yaml:"source" json:"source" jsonschema_description:"Acquisition input. Its lock fingerprint excludes preparation and destination metadata."`
	Platform     string                    `yaml:"platform,omitempty" json:"platform,omitempty" jsonschema_description:"Target software platform, independent of the runner operating system."`
	Arch         string                    `yaml:"arch,omitempty" json:"arch,omitempty" jsonschema_description:"Target architecture. Universal is an explicit vendor artifact containing multiple architectures."`
	Select       string                    `yaml:"select,omitempty" json:"select,omitempty" jsonschema_description:"Exact relative payload path within an archive. Required when several plausible payloads exist."`
	Verification Verification              `yaml:"verification,omitempty" json:"verification,omitzero" jsonschema_description:"Required verification subject and scope. Unsupported required checks block publication."`
	Artifacts    map[string]Artifact       `yaml:"artifacts,omitempty" json:"artifacts,omitempty" jsonschema_description:"Named reproducible artifacts derived from the selected source. Destinations select them by name."`
	Destinations map[string]map[string]any `yaml:"destinations,omitempty" json:"destinations,omitempty" jsonschema_description:"Named connections. Recipe destination entries own only explicitly present native metadata fields."`
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
	From            string            `yaml:"from,omitempty" json:"from,omitempty" jsonschema:"enum=source" jsonschema_description:"Input artifact. Omitted or source selects the prepared source in version 1."`
}

// Source specifies one provider. Environment variables are referenced by name.
type Source struct {
	Type       string   `yaml:"type" json:"type" jsonschema:"enum=http,enum=github,enum=file,enum=local" jsonschema_description:"Provider or destination implementation. Unknown values are rejected."`
	Include    []string `yaml:"include,omitempty" json:"include,omitempty" jsonschema_description:"Local files and glob patterns relative to this software-family file. Matched bytes, modes and symlinks determine content identity."`
	Base       string   `yaml:"-" json:"base,omitempty"`
	URL        string   `yaml:"url,omitempty" json:"url,omitempty" jsonschema_description:"Stable HTTP(S) URL without embedded credentials or expiring query parameters."`
	Path       string   `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Filesystem path. Source paths must stay within the Stemma project."`
	Repository string   `yaml:"repository,omitempty" json:"repository,omitempty" jsonschema_description:"GitHub repository in owner/name form."`
	Release    string   `yaml:"release,omitempty" json:"release,omitempty" jsonschema_description:"GitHub release tag or latest. New releases are resolved only during explicit updates or permitted missing lock resolution."`
	Asset      string   `yaml:"asset,omitempty" json:"asset,omitempty" jsonschema_description:"Exact GitHub release asset filename, avoiding ambiguous glob matches."`
	Filename   string   `yaml:"filename,omitempty" json:"filename,omitempty" jsonschema_description:"Retained artifact basename. Never a workspace or cache path."`
	Version    string   `yaml:"version,omitempty" json:"version,omitempty" jsonschema_description:"Declared software version when the source cannot expose one. It is metadata, not a content identity."`
	SHA256     string   `yaml:"sha256,omitempty" json:"sha256,omitempty" jsonschema_description:"Optional independently obtained SHA-256 requirement. Explicit refresh does not bypass this requirement."`
	TokenEnv   string   `yaml:"token_env,omitempty" json:"token_env,omitempty" jsonschema_description:"Name of the environment variable containing a bearer token. The token itself is never stored in a lockfile."`
}

// Verification declares the exact subject and checks required before publication.
type Verification struct {
	Subject           string `yaml:"subject,omitempty" json:"subject,omitempty" jsonschema:"enum=source,enum=payload" jsonschema_description:"Verify the original source or selected payload. A container signature does not verify an inner application."`
	Integrity         bool   `yaml:"integrity,omitempty" json:"integrity,omitempty" jsonschema_description:"Require all supported signed byte and hash checks for the selected subject."`
	Signature         bool   `yaml:"signature,omitempty" json:"signature,omitempty" jsonschema_description:"Require a cryptographically valid signature, separately from signer trust."`
	Resources         bool   `yaml:"resources,omitempty" json:"resources,omitempty" jsonschema_description:"Require sealed application resources. Unsupported nested-code layouts fail closed."`
	Identity          bool   `yaml:"identity,omitempty" json:"identity,omitempty" jsonschema_description:"Require authenticated signer identity using an implemented trust policy."`
	CertificateSHA256 string `yaml:"certificate_sha256,omitempty" json:"certificate_sha256,omitempty" jsonschema_description:"Exact SHA-256 pin of the signer DER certificate. PKG support checks the signature; this does not claim CA trust or revocation assessment."`
	Platform          bool   `yaml:"platform,omitempty" json:"platform,omitempty" jsonschema_description:"Require native OS policy assessment. Unsupported on portable verifier implementations."`
}

// Destination keeps connection settings separate from native recipe metadata.
type Destination struct {
	Type   string         `yaml:"type" json:"type" jsonschema:"enum=munki,enum=plugin,enum=jamf,enum=intune" jsonschema_description:"Provider or destination implementation. Unknown values are rejected."`
	Path   string         `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Filesystem path. Source paths must stay within the Stemma project."`
	Plugin string         `yaml:"plugin,omitempty" json:"plugin,omitempty" jsonschema_description:"Name of an explicitly trusted plugin declared in this project."`
	Config map[string]any `yaml:"config,omitempty" json:"config,omitempty" jsonschema_description:"Destination-specific connection configuration. Reference credential environment variables instead of embedding secrets."`
}

// Plugin identifies an explicitly trusted executable source for each host.
type Plugin struct {
	Trusted   bool              `yaml:"trusted" json:"trusted" jsonschema_description:"Explicit consent to execute this plugin. Checksums prove binary identity, not publisher trust."`
	Platforms map[string]Source `yaml:"platforms" json:"platforms" jsonschema_description:"Map runner OS/architecture to a plugin binary source, for example darwin/arm64."`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Parse rejects unknown fields, multiple documents, aliases and invalid composition.
func Parse(data []byte) (Project, error) {
	var p Project
	document, err := parseDocument(data, &p)
	if err != nil {
		return p, err
	}
	if len(p.Imports) != 0 {
		return p, errors.New("imports require loading a project file")
	}
	components, _ := document["components"].(map[string]any)
	recipes, _ := document["recipes"].(map[string]any)
	p.Recipes = map[string]Recipe{}
	if err := addRecipes(&p, recipes, components, "."); err != nil {
		return p, err
	}
	return p, p.Validate()
}

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

func addRecipes(p *Project, recipes, components map[string]any, base string) error {
	for name, raw := range recipes {
		if _, exists := p.Recipes[name]; exists {
			return fmt.Errorf("conflicting recipe ID %q", name)
		}
		resolved, err := resolve(raw, components, nil)
		if err != nil {
			return fmt.Errorf("recipe %s: %w", name, err)
		}
		encoded, err := yaml.Marshal(resolved)
		if err != nil {
			return err
		}
		var recipe Recipe
		if err := decodeStrict(encoded, &recipe); err != nil {
			return err
		}
		if recipe.Source.Type == "file" {
			filename := recipe.Source.Path
			if filename == "" || strings.HasPrefix(filename, "/") || strings.ContainsAny(filename, "\\:\x00\r\n") {
				return fmt.Errorf("recipe %s: file source path must be relative", name)
			}
			recipe.Source.Path = path.Join(base, filename)
			if !safeRelative(recipe.Source.Path) {
				return fmt.Errorf("recipe %s: file source path must remain within the project", name)
			}
		}
		if recipe.Source.Type == "local" {
			recipe.Source.Base = base
		}
		p.Recipes[name] = recipe
	}
	return nil
}

func decodeStrict(data []byte, value any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(value)
}

func checkNode(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("YAML aliases are not supported; use recipe components")
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
		return nil, errors.New("recipe must be a mapping")
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
	if p.Version != 1 {
		return errors.New("version must be 1")
	}
	if !namePattern.MatchString(p.Project) {
		return errors.New("project must be a stable name containing letters, digits, dots, underscores or hyphens")
	}
	if len(p.Recipes) == 0 {
		return errors.New("recipes must not be empty")
	}
	for name, r := range p.Recipes {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid recipe name %q", name)
		}
		if err := r.Source.Validate(); err != nil {
			return fmt.Errorf("recipe %s: %w", name, err)
		}
		if r.Platform != "" && r.Platform != "darwin" && r.Platform != "linux" && r.Platform != "windows" {
			return fmt.Errorf("recipe %s: unsupported platform", name)
		}
		if r.Arch != "" && r.Arch != "amd64" && r.Arch != "arm64" && r.Arch != "universal" {
			return fmt.Errorf("recipe %s: unsupported architecture", name)
		}
		if r.Select != "" && !filepath.IsLocal(filepath.FromSlash(r.Select)) {
			return fmt.Errorf("recipe %s: select must be a relative path", name)
		}
		if v := r.Verification; v.Subject != "" && v.Subject != "source" && v.Subject != "payload" {
			return fmt.Errorf("recipe %s: verification subject must be source or payload", name)
		}
		if r.Verification.CertificateSHA256 != "" && !ValidDigest(r.Verification.CertificateSHA256) {
			return fmt.Errorf("recipe %s: invalid certificate SHA-256", name)
		}
		for artifact, value := range r.Artifacts {
			if !namePattern.MatchString(artifact) {
				return fmt.Errorf("recipe %s: invalid artifact name %q", name, artifact)
			}
			if err := value.Validate(); err != nil {
				return fmt.Errorf("recipe %s artifact %s: %w", name, artifact, err)
			}
		}
		for destination, metadata := range r.Destinations {
			if _, exists := p.Destinations[destination]; !exists {
				return fmt.Errorf("recipe %s: unknown destination %q", name, destination)
			}
			if selected, exists := metadata["artifact"]; exists {
				artifact, ok := selected.(string)
				if _, exists := r.Artifacts[artifact]; !ok || !exists {
					return fmt.Errorf("recipe %s destination %s: artifact must name a configured artifact", name, destination)
				}
			}
		}
	}
	for name, d := range p.Destinations {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid destination name %q", name)
		}
		switch d.Type {
		case "munki":
			if d.Path == "" || d.Plugin != "" || len(d.Config) != 0 {
				return fmt.Errorf("destination %s: munki requires only path", name)
			}
		case "plugin":
			if _, exists := p.Plugins[d.Plugin]; !exists || d.Path != "" {
				return fmt.Errorf("destination %s: plugin must name a configured plugin", name)
			}
		case "intune", "jamf":
			if d.Path != "" || d.Plugin != "" {
				return fmt.Errorf("destination %s: use config for connection settings", name)
			}
		default:
			return fmt.Errorf("destination %s: unsupported type %q", name, d.Type)
		}
	}
	for name, plugin := range p.Plugins {
		if !namePattern.MatchString(name) || !plugin.Trusted || len(plugin.Platforms) == 0 {
			return fmt.Errorf("plugin %s: requires a valid name, trusted: true and platform sources", name)
		}
		for platform, source := range plugin.Platforms {
			if !regexp.MustCompile(`^(darwin|linux|windows)/(amd64|arm64)$`).MatchString(platform) {
				return fmt.Errorf("plugin %s: invalid platform %q", name, platform)
			}
			if err := source.Validate(); err != nil {
				return fmt.Errorf("plugin %s: %w", name, err)
			}
		}
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
		if !safeRelative(s.Path) || s.URL != "" || s.Repository != "" || s.Release != "" || s.Asset != "" || s.TokenEnv != "" {
			return errors.New("file source requires a project-relative path only")
		}
	case "local":
		if len(s.Include) == 0 || s.Path != "" || s.URL != "" || s.Repository != "" || s.Release != "" || s.Asset != "" || s.TokenEnv != "" || (s.Base != "" && !safeRelative(s.Base)) {
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
	if a.Type != "pkg" || (a.From != "" && a.From != "source") {
		return errors.New("artifact type must be pkg and from must be source or omitted")
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
