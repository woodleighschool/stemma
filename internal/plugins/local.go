package plugins

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/oras-project/oras-go/v3/registry/remote/properties"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Fingerprint identifies the declaration a lock entry answers.
func Fingerprint(declaration config.Plugin) string {
	return config.Fingerprint(struct {
		Image, Path, Entrypoint string
	}{declaration.Image, declaration.Path, declaration.Entrypoint})
}

// Pin names the code a declaration selects: the digest an image names, or the
// digest of the lock entry that answers the declaration. It is empty when the
// declaration is not locked.
func Pin(declaration config.Plugin, entry Entry) string {
	if _, digest, pinned := strings.Cut(declaration.Image, "@"); pinned {
		return digest
	}
	if entry.Declaration == Fingerprint(declaration) {
		return entry.Digest
	}
	return ""
}

// Load selects a plugin's code and returns the lock entry that selects it. An
// image that names its digest needs no entry. A tag or local path uses the
// previous entry for its declaration; with resolve, a missing or stale entry
// is resolved instead: the tag from the registry, the path from its current
// files. Local files are checked before any plugin code runs.
func (s *Store) Load(ctx context.Context, root string, declaration config.Plugin, previous Entry, resolve bool) (Bundle, Entry, error) {
	if err := declaration.Validate(); err != nil {
		return Bundle{}, Entry{}, err
	}
	fingerprint := Fingerprint(declaration)
	if previous.Declaration != fingerprint {
		previous = Entry{}
	}
	if declaration.Path == "" {
		ref, err := properties.NewReference(declaration.Image)
		if err != nil {
			return Bundle{}, Entry{}, err
		}
		if ref.Digest != "" {
			bundle, err := s.Acquire(ctx, declaration.Image, ref.Digest)
			return bundle, Entry{}, err
		}
		entry := previous
		if entry.Digest == "" {
			if !resolve {
				return Bundle{}, Entry{}, errors.New("image tag is not locked; run stemma plugins update")
			}
			digest, err := s.Resolve(ctx, declaration.Image)
			if err != nil {
				return Bundle{}, Entry{}, err
			}
			entry = Entry{Declaration: fingerprint, Digest: digest}
		}
		bundle, err := s.Acquire(ctx, declaration.Image, entry.Digest)
		return bundle, entry, err
	}
	path := declaration.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	manager := source.New(s.cache, filepath.Dir(path), true)
	current, err := manager.Resolve(ctx, plugin.Input{Resolver: "file", Config: map[string]any{"path": filepath.Base(path)}})
	if err != nil {
		return Bundle{}, Entry{}, err
	}
	if !current.Content.Tree && declaration.Entrypoint != "" {
		return Bundle{}, Entry{}, errors.New("entrypoint requires a directory; path already selects an executable")
	}
	entry := Entry{Declaration: fingerprint, Digest: "sha256:" + current.Content.Artifact.SHA256}
	switch {
	case resolve || entry == previous:
	case previous.Digest == "":
		return Bundle{}, Entry{}, errors.New("local files are not locked; run stemma plugins update")
	default:
		return Bundle{}, Entry{}, errors.New("local files changed since they were locked; run stemma plugins update")
	}
	name := declaration.Entrypoint
	if name == "" {
		name = entrypoint(s.platform.OS)
		if !current.Content.Tree {
			name = current.Content.Filename
		}
	}
	identity := config.Fingerprint(struct {
		Content    source.Content
		Entrypoint string
	}{current.Content, name})
	bundle := Bundle{Manifest: identity, Artifact: current.Content.Artifact, Local: &current.Content, Entrypoint: name}
	return bundle, entry, nil
}
