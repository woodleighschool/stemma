// Package source resolves opaque declarations into immutable cached content.
package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

// Content identifies bytes and their filesystem representation. Tree bytes are a
// canonical TAR containing every selected child mode and confined symlink target.
type Content struct {
	Artifact cas.Ref `json:"artifact" yaml:"artifact"`
	Filename string  `json:"filename" yaml:"filename"`
	Tree     bool    `json:"tree,omitempty" yaml:"tree,omitempty"`
	Mode     uint32  `json:"mode" yaml:"mode"`
}

// Entry is the common lock envelope. Observation belongs to the named resolver
// and must contain only stable, nonsecret data needed to fetch this exact input.
type Entry struct {
	Version         int             `json:"version" yaml:"version"`
	Resolver        string          `json:"resolver" yaml:"resolver"`
	ResolverVersion string          `json:"resolver_version" yaml:"resolver_version"`
	Declaration     string          `json:"declaration" yaml:"declaration"`
	ResolvedAt      time.Time       `json:"resolved_at" yaml:"resolved_at"`
	Observation     json.RawMessage `json:"observation" yaml:"observation"`
	Content         Content         `json:"content" yaml:"content"`
}

// Resolution returns a leased resolver output for import into the shared cache.
type Resolution struct {
	Observation json.RawMessage
	Artifact    plugin.Artifact
}

// Resolver owns its declaration and stable observation. Local resolvers are
// reobserved even with a warm cache. Fingerprint must exclude credentials.
// Callbacks keep returned artifact paths available until the manager returns.
type Resolver struct {
	Version     string
	Local       bool
	Fingerprint func(plugin.Input) (string, error)
	Resolve     func(context.Context, plugin.Input) (Resolution, error)
	FetchLocked func(context.Context, plugin.Input, json.RawMessage) (plugin.Artifact, error)
}

// Manager owns acquisition and cached content, independently of resource kinds.
type Manager struct {
	Store     *cas.Store
	Root      string
	Client    *http.Client
	Offline   bool
	Resolvers map[string]Resolver
}

// New creates a manager with bounded HTTP lifetimes and credential-safe redirects.
func New(store *cas.Store, root string, offline bool) *Manager {
	return &Manager{Store: store, Root: root, Offline: offline, Resolvers: map[string]Resolver{}, Client: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if !sameOrigin(req.URL, via[0].URL) || !sameOrigin(req.URL, via[len(via)-1].URL) {
			stripPrivateHeaders(req.Header)
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("refusing HTTPS downgrade")
		}
		return nil
	}}}
}

// Register adds a resolver without shadowing native or previously registered inputs.
func (m *Manager) Register(name string, resolver Resolver) error {
	if !plugin.ValidOperationName(name) || resolver.Version == "" || resolver.Resolve == nil || resolver.FetchLocked == nil {
		return errors.New("resolver registration requires a name, version and acquisition callbacks")
	}
	if _, err := m.resolver(name); err == nil {
		return fmt.Errorf("resolver %s is already registered", name)
	}
	if m.Resolvers == nil {
		m.Resolvers = map[string]Resolver{}
	}
	m.Resolvers[name] = resolver
	return nil
}

func (m *Manager) resolver(name string) (Resolver, error) {
	if resolver, ok := m.Resolvers[name]; ok {
		if resolver.Version == "" || resolver.Resolve == nil || resolver.FetchLocked == nil {
			return Resolver{}, fmt.Errorf("resolver %s has an incomplete contract", name)
		}
		return resolver, nil
	}
	switch name {
	case "http", "github":
		return Resolver{Version: "1"}, nil
	case "file", "local":
		return Resolver{Version: "1", Local: true}, nil
	default:
		return Resolver{}, fmt.Errorf("unsupported input resolver %q", name)
	}
}

// Declaration validates the declaration and returns its resolver version and
// credential-independent hash, without resolving or acquiring content.
func (m *Manager) Declaration(input plugin.Input) (string, string, error) {
	if input.Resource != nil {
		return "", "", errors.New("resource output references must be resolved by the scheduler")
	}
	resolver, err := m.resolver(input.Resolver)
	if err != nil {
		return "", "", err
	}
	var digest string
	switch {
	case resolver.Resolve == nil:
		s, err := native(input)
		if err != nil {
			return "", "", err
		}
		s.Token = ""
		for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
			delete(s.Headers, name)
		}
		digest, err = fingerprint(s)
		if err != nil {
			return "", "", err
		}
	case resolver.Fingerprint != nil:
		digest, err = resolver.Fingerprint(input)
	case resolver.Local:
		digest, err = fingerprint(struct {
			Config map[string]any
			Base   string
		}{input.Config, input.Base})
	default:
		digest, err = fingerprint(input.Config)
	}
	if err != nil {
		return "", "", err
	}
	if !validDigest(digest) {
		return "", "", errors.New("resolver returned an invalid declaration hash")
	}
	return resolver.Version, digest, nil
}

// IsLocal reports whether the resolver must reobserve filesystem or other local
// inputs rather than trusting a previously cached object.
func (m *Manager) IsLocal(name string) bool {
	resolver, err := m.resolver(name)
	return err == nil && resolver.Local
}

// Resolve observes a declaration once and imports its exact content into CAS.
func (m *Manager) Resolve(ctx context.Context, input plugin.Input) (Entry, error) {
	version, declaration, err := m.Declaration(input)
	if err != nil {
		return Entry{}, err
	}
	resolver, _ := m.resolver(input.Resolver)
	if m.Offline && !resolver.Local {
		return Entry{}, errors.New("offline mode cannot resolve inputs")
	}
	entry := Entry{Version: 1, Resolver: input.Resolver, ResolverVersion: version, Declaration: declaration, ResolvedAt: time.Now().UTC().Truncate(time.Second)}
	if resolver.Resolve == nil {
		entry.Content, entry.Observation, err = m.resolveNative(ctx, input)
	} else {
		var result Resolution
		result, err = resolver.Resolve(ctx, input)
		if err == nil {
			entry.Content, err = m.importArtifact(ctx, result.Artifact)
			entry.Observation = result.Observation
		}
	}
	if err != nil {
		return Entry{}, err
	}
	entry.Observation, err = canonicalJSON(entry.Observation)
	if err != nil {
		return Entry{}, fmt.Errorf("resolver observation: %w", err)
	}
	return entry, entry.Validate()
}

// FetchLocked uses verified cached content or fetches from the locked observation.
// It never refreshes remote discovery; local inputs are always rehashed.
func (m *Manager) FetchLocked(ctx context.Context, input plugin.Input, entry Entry) (bool, error) {
	version, declaration, err := m.Declaration(input)
	if err != nil {
		return false, err
	}
	if err := entry.Validate(); err != nil {
		return false, err
	}
	if entry.Resolver != input.Resolver || entry.ResolverVersion != version || entry.Declaration != declaration {
		return false, errors.New("invalid or stale input lock")
	}
	cached := m.Store.Verify(ctx, entry.Content.Artifact)
	resolver, _ := m.resolver(input.Resolver)
	if resolver.Local {
		current, err := m.Resolve(ctx, input)
		if err != nil {
			return false, err
		}
		current.ResolvedAt = entry.ResolvedAt
		if !current.Equal(entry) {
			return false, errors.New("local input changed from its locked content")
		}
		return cached == nil, nil
	}
	if cached == nil {
		return true, nil
	}
	if !errors.Is(cached, os.ErrNotExist) {
		return false, cached
	}
	if m.Offline {
		return false, fmt.Errorf("offline cache miss for %s", entry.Content.Filename)
	}
	var content Content
	if resolver.FetchLocked == nil {
		content, err = m.fetchNative(ctx, input, entry)
	} else {
		var artifact plugin.Artifact
		artifact, err = resolver.FetchLocked(ctx, input, entry.Observation)
		if err == nil {
			content, err = m.importArtifact(ctx, artifact)
		}
	}
	if err != nil {
		return false, err
	}
	if content != entry.Content {
		return false, errors.New("fetched content differs from the input lock")
	}
	return false, nil
}

func (m *Manager) importArtifact(ctx context.Context, artifact plugin.Artifact) (Content, error) {
	if !validFilename(artifact.Filename) {
		return Content{}, errors.New("resolver artifact requires a safe filename")
	}
	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return Content{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Content{}, errors.New("resolver artifact root must not be a symlink")
	}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return Content{}, err
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return Content{}, err
	}
	if info.IsDir() != artifact.Tree || !info.IsDir() && !info.Mode().IsRegular() {
		return Content{}, errors.New("resolver artifact type differs from its descriptor")
	}
	if err := archive.CheckMetadata(file, info); err != nil {
		return Content{}, err
	}
	content := Content{Filename: artifact.Filename, Tree: artifact.Tree, Mode: uint32(info.Mode().Perm())}
	if artifact.Tree {
		root, err := os.OpenRoot(artifact.Path)
		if err != nil {
			return Content{}, err
		}
		defer func() { _ = root.Close() }()
		content.Artifact, err = m.importTree(ctx, root, nil, artifact.SHA256)
		if err != nil {
			return Content{}, err
		}
	} else {
		if (artifact.SHA256 != "" || artifact.Size != 0) && info.Size() != artifact.Size {
			return Content{}, errors.New("resolver artifact size differs from its descriptor")
		}
		content.Artifact, err = m.Store.Import(ctx, file, artifact.SHA256)
		if err != nil {
			return Content{}, err
		}
	}
	if artifact.Size != 0 && artifact.Size != content.Artifact.Size {
		return Content{}, errors.New("resolver artifact size differs from imported content")
	}
	return content, nil
}

// Validate checks only the shared lock envelope; resolver-owned observations are
// interpreted by their resolver when content must be fetched again.
func (entry Entry) Validate() error {
	if entry.Version != 1 || !plugin.ValidOperationName(entry.Resolver) || entry.ResolverVersion == "" || !validDigest(entry.Declaration) || entry.ResolvedAt.IsZero() {
		return errors.New("unsupported or incomplete input lock envelope")
	}
	if !validFilename(entry.Content.Filename) || !validDigest(entry.Content.Artifact.SHA256) || entry.Content.Artifact.Size < 0 || entry.Content.Artifact.Size > cas.MaxObjectSize || entry.Content.Mode > 0o777 {
		return errors.New("invalid locked input content")
	}
	if !json.Valid(entry.Observation) {
		return errors.New("invalid resolver observation")
	}
	return nil
}

// Equal compares semantic lock state, including resolver-owned JSON observations.
func (entry Entry) Equal(other Entry) bool {
	left, err := canonicalJSON(entry.Observation)
	if err != nil {
		return false
	}
	right, err := canonicalJSON(other.Observation)
	return err == nil && entry.Version == other.Version && entry.Resolver == other.Resolver && entry.ResolverVersion == other.ResolverVersion && entry.Declaration == other.Declaration && entry.ResolvedAt.Equal(other.ResolvedAt) && entry.Content == other.Content && bytes.Equal(left, right)
}

// MarshalYAML preserves resolver JSON as ordinary YAML objects, including exact
// integer values, rather than serializing RawMessage as a byte array.
func (entry Entry) MarshalYAML() (any, error) {
	type plain Entry
	data, err := json.Marshal(plain(entry))
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	var blockStyle func(*yaml.Node)
	blockStyle = func(node *yaml.Node) {
		node.Style = 0
		for _, child := range node.Content {
			blockStyle(child)
		}
	}
	blockStyle(&node)
	return node.Content[0], nil
}

func (entry *Entry) UnmarshalYAML(node *yaml.Node) error {
	value, err := yamlJSON(node)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	type plain Entry
	if err := decode(data, (*plain)(entry)); err != nil {
		return err
	}
	entry.Observation, err = canonicalJSON(entry.Observation)
	return err
}

func yamlJSON(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		values := map[string]any{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag != "!!str" || key.Value == "<<" {
				return nil, errors.New("lock mapping keys must be strings")
			}
			if _, ok := values[key.Value]; ok {
				return nil, fmt.Errorf("duplicate lock field %q", key.Value)
			}
			value, err := yamlJSON(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			values[key.Value] = value
		}
		return values, nil
	case yaml.SequenceNode:
		values := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlJSON(child)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case yaml.DocumentNode, yaml.AliasNode, yaml.StreamNode:
		return nil, errors.New("input locks do not support nested documents or aliases")
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			var value bool
			err := node.Decode(&value)
			return value, err
		case "!!int", "!!float":
			return json.Number(node.Value), nil
		case "!!str", "!!timestamp":
			return node.Value, nil
		}
	}
	return nil, errors.New("unsupported YAML value in input lock")
}

func canonicalJSON(data json.RawMessage) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected one JSON value")
	}
	return json.Marshal(value)
}

func decode(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}
func fingerprint(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
func validDigest(value string) bool {
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size && strings.ToLower(value) == value
}
