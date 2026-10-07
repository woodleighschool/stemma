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

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

// Discovery is a resolver's current observation. Immutable promises that the
// observation always fetches the same bytes.
type Discovery struct {
	Content     *Content
	Version     string
	ContentRoot string
	Evidence    map[string]json.RawMessage
	Observation json.RawMessage
	Immutable   bool
}

// Resolver owns its declaration and stable observation. Local resolvers are
// reobserved even with a warm cache. Fingerprint must exclude credentials.
// Identity names the implementation in the source index and defaults to
// Version. Acquire keeps returned artifact paths available until the manager
// returns.
type Resolver struct {
	Version     string
	Identity    string
	Local       bool
	Fingerprint func(plugin.Input) (string, error)
	Discover    func(context.Context, plugin.Input) (Discovery, error)
	Schema      *jsonschema.Schema
	Acquire     func(context.Context, plugin.Input, json.RawMessage) (Acquisition, error)
	Changed     func(plugin.Input, []string) (bool, error)
}

// record is what the source index keeps for one observation: the content a
// fetch produced and, for HTTP, the validators to ask about it again.
type record struct {
	Content      Content                    `json:"content"`
	Evidence     map[string]json.RawMessage `json:"evidence,omitempty"`
	ETag         string                     `json:"etag,omitempty"`
	LastModified string                     `json:"last_modified,omitempty"`
}

// Manager owns acquisition and cached content, independently of resource kinds.
type Manager struct {
	metadata  *metadataCache
	Store     *cas.Store
	Root      string
	Client    *http.Client
	Offline   bool
	resolvers map[string]Resolver
}

// New creates a manager with bounded HTTP lifetimes and credential-safe redirects.
func New(store *cas.Store, root string, offline bool) *Manager {
	manager := &Manager{metadata: &metadataCache{}, Store: store, Root: root, Offline: offline, Client: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: checkRedirect(false)}}
	manager.resolvers = builtins(manager)
	return manager
}

// Register adds a resolver without shadowing native or previously registered inputs.
func (m *Manager) Register(name string, resolver Resolver) error {
	if !plugin.ValidOperationName(name) || resolver.Version == "" || resolver.Fingerprint == nil || resolver.Discover == nil || resolver.Acquire == nil {
		return errors.New("resolver registration requires a name, version and acquisition callbacks")
	}
	if _, err := m.resolver(name); err == nil {
		return fmt.Errorf("resolver %s is already registered", name)
	}
	if m.resolvers == nil {
		m.resolvers = map[string]Resolver{}
	}
	m.resolvers[name] = resolver
	return nil
}

func (m *Manager) resolver(name string) (Resolver, error) {
	if resolver, ok := m.resolvers[name]; ok {
		if resolver.Version == "" || resolver.Fingerprint == nil || resolver.Discover == nil || resolver.Acquire == nil {
			return Resolver{}, fmt.Errorf("resolver %s has an incomplete contract", name)
		}
		return resolver, nil
	}
	return Resolver{}, fmt.Errorf("unsupported input resolver %q", name)
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
	digest, err := resolver.Fingerprint(input)
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

// Resolve observes a declaration without a reviewed entry.
func (m *Manager) Resolve(ctx context.Context, input plugin.Input) (Entry, error) {
	entry, _, err := m.Refresh(ctx, input, Entry{})
	return entry, err
}

// Refresh observes a declaration again and reports whether the cache already
// held its content. An immutable observation takes its content from the
// source index, or from the reviewed entry when it records the same
// observation; anything else is fetched, and a mutable HTTP source is asked
// conditionally with the validators the index kept. Local inputs are read in
// full every time.
func (m *Manager) Refresh(ctx context.Context, input plugin.Input, locked Entry) (Entry, bool, error) {
	version, declaration, err := m.Declaration(input)
	if err != nil {
		return Entry{}, false, err
	}
	resolver, _ := m.resolver(input.Resolver)
	if m.Offline && !resolver.Local {
		return Entry{}, false, errors.New("offline mode cannot resolve inputs")
	}
	found, err := resolver.Discover(ctx, input)
	if err != nil {
		return Entry{}, false, err
	}
	entry := Entry{Version: 1, Resolver: input.Resolver, ResolverVersion: version, Declaration: declaration, InputVersion: found.Version, ContentRoot: found.ContentRoot}
	entry.Evidence, err = canonicalEvidence(found.Evidence)
	if err != nil {
		return Entry{}, false, err
	}
	entry.Observation, err = canonicalJSON(found.Observation)
	if err != nil {
		return Entry{}, false, fmt.Errorf("resolver observation: %w", err)
	}
	if found.Content != nil && !resolver.Local {
		entry.Content = *found.Content
		if err := entry.Validate(); err != nil {
			return Entry{}, false, err
		}
		return entry, m.Store.Reuse(ctx, entry.Content.SHA256), nil
	}
	if resolver.Local {
		current, _, err := m.acquire(ctx, resolver, input, entry, nil)
		if err != nil {
			return Entry{}, false, err
		}
		entry.Content = current.Content
		if found.Evidence == nil {
			entry.Evidence = current.Evidence
		}
		return entry, false, entry.Validate()
	}
	key, err := entrySourceKey(input.Resolver, resolver, declaration, entry)
	if err != nil {
		return Entry{}, false, err
	}
	var known record
	recalled := m.Store.RecallSource(ctx, key, &known) && known.Content.valid()
	switch {
	case found.Immutable && recalled:
		entry.Content = known.Content
		if found.Evidence == nil {
			entry.Evidence = known.Evidence
		}
		return entry, m.Store.Reuse(ctx, entry.Content.SHA256), entry.Validate()
	case found.Immutable && locked.Validate() == nil && locked.Resolver == entry.Resolver && locked.ResolverVersion == version && locked.Declaration == declaration && sameJSON(locked.Observation, entry.Observation):
		entry.Content = locked.Content
		if found.Evidence == nil {
			entry.Evidence = locked.Evidence
		}
		return entry, m.Store.Reuse(ctx, locked.Content.SHA256), entry.Validate()
	}
	var previous *record
	if recalled {
		previous = &known
	}
	current, reused, err := m.acquire(ctx, resolver, input, entry, previous)
	if err != nil {
		return Entry{}, false, err
	}
	if err := m.Store.RememberSource(ctx, key, current, current.Content.SHA256); err != nil {
		return Entry{}, false, err
	}
	entry.Content = current.Content
	if found.Evidence == nil {
		entry.Evidence = current.Evidence
	}
	return entry, reused && m.Store.Reuse(ctx, entry.Content.SHA256), entry.Validate()
}

// FetchLocked uses verified cached content or fetches the locked observation
// again. It never refreshes remote discovery; local inputs are always rehashed.
// The source index keeps whatever the fetch returned, so a source that moved
// on is proposed by the next refresh without another download.
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
	cached := m.Store.VerifyDigest(ctx, entry.Content.SHA256)
	resolver, _ := m.resolver(input.Resolver)
	if resolver.Local {
		current, err := m.Resolve(ctx, input)
		if err != nil {
			return false, err
		}
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
	current, _, err := m.acquire(ctx, resolver, input, entry, nil)
	if err != nil {
		return false, err
	}
	key, err := entrySourceKey(input.Resolver, resolver, declaration, entry)
	if err != nil {
		return false, err
	}
	if err := m.Store.RememberSource(ctx, key, current, current.Content.SHA256); err != nil {
		return false, err
	}
	if current.Content.SHA256 != entry.Content.SHA256 || current.Content.Tree != entry.Content.Tree {
		return false, errors.New("fetched content differs from the input lock")
	}
	return false, nil
}

func entrySourceKey(name string, resolver Resolver, declaration string, entry Entry) (string, error) {
	return sourceKey(name, resolver, declaration, entry.Observation)
}

// sourceKey identifies an observation in the source index. The resolver
// implementation is part of it, so a new plugin build fetches again rather
// than trusting what an older build fetched.
func sourceKey(name string, resolver Resolver, declaration string, observation json.RawMessage) (string, error) {
	identity := resolver.Identity
	if identity == "" {
		identity = resolver.Version
	}
	canonical, err := canonicalJSON(observation)
	if err != nil {
		return "", err
	}
	return fingerprint(struct {
		Resolver, Identity, Declaration string
		Observation                     json.RawMessage
	}{name, identity, declaration, canonical})
}

func sameJSON(a, b json.RawMessage) bool {
	left, err := canonicalJSON(a)
	if err != nil {
		return false
	}
	right, err := canonicalJSON(b)
	return err == nil && bytes.Equal(left, right)
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
	content := Content{Filename: artifact.Filename, Tree: artifact.Tree, Mode: uint32(info.Mode().Perm())}
	if artifact.Tree {
		root, err := os.OpenRoot(artifact.Path)
		if err != nil {
			return Content{}, err
		}
		defer func() { _ = root.Close() }()
		var object cas.Ref
		object, err = m.importTree(ctx, root, nil, artifact.SHA256)
		content.SHA256 = object.SHA256
		if err != nil {
			return Content{}, err
		}
	} else {
		if (artifact.SHA256 != "" || artifact.Size != 0) && info.Size() != artifact.Size {
			return Content{}, errors.New("resolver artifact size differs from its descriptor")
		}
		var object cas.Ref
		object, err = m.Store.Import(ctx, file, artifact.SHA256)
		content.SHA256 = object.SHA256
		if err != nil {
			return Content{}, err
		}
	}
	measured, err := m.Store.Lookup(content.SHA256)
	if err != nil {
		return Content{}, err
	}
	if artifact.Size != 0 && artifact.Size != measured.Size {
		return Content{}, errors.New("resolver artifact size differs from imported content")
	}
	return content, nil
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
