// Package plugins snapshots local executables and acquires pinned OCI bundles.
package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/source"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const ArtifactType = "application/vnd.stemma.plugin.v1"
const BundleType = "application/vnd.stemma.plugin.bundle.v1.tar+zstd"
const maxMetadataSize = 1 << 20
const maxPlatforms = 64

var zstdMagic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}

// Entry records a local content observation or an OCI release index.
type Entry struct {
	Path       string        `json:"path,omitempty" yaml:"path,omitempty"`
	Entrypoint string        `json:"entrypoint,omitempty" yaml:"entrypoint,omitempty"`
	Local      *source.Entry `json:"local,omitempty" yaml:"local,omitempty"`
	Image      string        `json:"image,omitempty" yaml:"image,omitempty"`
	Digest     string        `json:"digest,omitempty" yaml:"digest,omitempty"`
	Size       int64         `json:"size,omitempty" yaml:"size,omitempty"`
}

// Bundle identifies the entire selected package, including executable resources.
type Bundle struct {
	Manifest   string
	Artifact   cas.Ref
	Local      *source.Content
	Entrypoint string
}

// Store keeps registry content in the shared disposable cache. Callers hold a lease.
type Store struct {
	cache    *cas.Store
	offline  bool
	platform ocispec.Platform
	registry func(string) (oras.ReadOnlyTarget, error)
}

func New(cache *cas.Store, offline bool) *Store {
	registry := func(image string) (oras.ReadOnlyTarget, error) {
		repo, err := repository(image)
		if err != nil {
			return nil, err
		}
		return repo, nil
	}
	return &Store{cache: cache, offline: offline, platform: ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}, registry: registry}
}

func repository(image string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(image)
	if err != nil {
		return nil, err
	}
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, err
	}
	client := *retry.DefaultClient
	client.Timeout = 15 * time.Minute
	repo.Client = &auth.Client{Client: &client, Cache: auth.NewCache(), Credential: credentials.Credential(store)}
	repo.MaxMetadataBytes = maxMetadataSize
	return repo, nil
}

// Resolve contacts the registry only for an explicit installation or update.
func (s *Store) Resolve(ctx context.Context, image string) (Entry, error) {
	if s.offline {
		return Entry{}, errors.New("cannot resolve a plugin image offline")
	}
	repo, err := s.registry(image)
	if err != nil {
		return Entry{}, err
	}
	desc, err := repo.Resolve(ctx, image)
	if err != nil {
		return Entry{}, err
	}
	if desc.MediaType != ocispec.MediaTypeImageIndex {
		return Entry{}, errors.New("plugin image must be an OCI platform index")
	}
	entry := Entry{Image: image, Digest: desc.Digest.String(), Size: desc.Size}
	return entry, entry.Validate(image)
}

func (e Entry) Validate(image string) error {
	if e.Image != image || e.Image == "" || e.Path != "" || e.Local != nil || e.Entrypoint != "" {
		return errors.New("plugin image is missing or stale in the lockfile; run stemma plugins install")
	}
	_, err := reference(ocispec.Descriptor{Digest: digest.Digest(e.Digest), Size: e.Size}, maxMetadataSize)
	return err
}

// Acquire uses the locked index even if the configured tag has moved. It fetches
// only the runner's manifest, config and bundle; cached bytes are verified first.
func (s *Store) Acquire(ctx context.Context, image string, entry Entry) (Bundle, error) {
	if err := entry.Validate(image); err != nil {
		return Bundle{}, err
	}
	fetcher := &fetcher{store: s, image: image}
	var index ocispec.Index
	if err := fetcher.metadata(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.Digest(entry.Digest), Size: entry.Size}, &index); err != nil {
		return Bundle{}, err
	}
	if index.SchemaVersion != 2 || index.MediaType != ocispec.MediaTypeImageIndex || len(index.Manifests) == 0 || len(index.Manifests) > maxPlatforms {
		return Bundle{}, errors.New("invalid plugin platform index")
	}
	var selected ocispec.Descriptor
	seen := map[string]bool{}
	for _, desc := range index.Manifests {
		if desc.MediaType != ocispec.MediaTypeImageManifest || desc.Platform == nil {
			return Bundle{}, errors.New("plugin index requires platform-specific manifests")
		}
		key, err := runnerPlatform(*desc.Platform)
		if err != nil {
			return Bundle{}, err
		}
		if seen[key] {
			return Bundle{}, fmt.Errorf("plugin index has more than one bundle for %s", key)
		}
		seen[key] = true
		if desc.Platform.OS == s.platform.OS && desc.Platform.Architecture == s.platform.Architecture {
			selected = desc
		}
	}
	if selected.Digest == "" {
		return Bundle{}, fmt.Errorf("plugin has no bundle for %s/%s", s.platform.OS, s.platform.Architecture)
	}
	var manifest ocispec.Manifest
	if err := fetcher.metadata(ctx, selected, &manifest); err != nil {
		return Bundle{}, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != ocispec.MediaTypeImageManifest || manifest.ArtifactType != ArtifactType || len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != BundleType {
		return Bundle{}, errors.New("plugin manifest requires one Stemma tar.zst bundle")
	}
	if _, err := reference(manifest.Config, maxMetadataSize); err != nil {
		return Bundle{}, err
	}
	config, err := fetcher.Fetch(ctx, manifest.Config)
	if err != nil {
		return Bundle{}, err
	}
	if err := config.Close(); err != nil {
		return Bundle{}, err
	}
	layer := manifest.Layers[0]
	ref, err := reference(layer, cas.MaxObjectSize)
	if err != nil {
		return Bundle{}, err
	}
	blob, err := fetcher.Fetch(ctx, layer)
	if err != nil {
		return Bundle{}, err
	}
	defer func() { _ = blob.Close() }()
	var magic [4]byte
	if _, err := io.ReadFull(blob, magic[:]); err != nil || magic != zstdMagic {
		return Bundle{}, errors.New("plugin bundle is not a Zstandard archive")
	}
	return Bundle{Manifest: selected.Digest.String(), Artifact: ref}, nil
}

// Materialize extracts into a new leased workspace and returns its fixed entrypoint.
func (s *Store) Materialize(ctx context.Context, bundle Bundle, destination string) (executable string, err error) {
	if err := s.cache.Verify(ctx, bundle.Artifact); err != nil {
		return "", err
	}
	path, err := s.cache.Path(bundle.Artifact)
	if err != nil {
		return "", err
	}
	if bundle.Local != nil && !bundle.Local.Tree {
		if err := s.cache.Materialize(ctx, bundle.Artifact, filepath.Join(destination, bundle.Entrypoint)); err != nil {
			return "", err
		}
	} else if err := archive.Extract(ctx, path, destination); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	name := entrypoint(s.platform.OS)
	if bundle.Entrypoint != "" {
		name = bundle.Entrypoint
	}
	executable, err = regularFile(destination, name)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(executable, 0o700); err != nil {
		return "", err
	}
	return executable, nil
}

// entrypoint names the executable at the root of a bundle for goos.
func entrypoint(goos string) string {
	if goos == "windows" {
		return "plugin.exe"
	}
	return "plugin"
}

func regularFile(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("plugin bundle entrypoint: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("plugin entrypoint must be a regular file")
	}
	return path, nil
}

// runnerPlatform names a bundle's platform. Stemma selects a bundle by the
// runner's OS and architecture alone, so any finer platform field would make
// that selection ambiguous.
func runnerPlatform(p ocispec.Platform) (string, error) {
	key := p.OS + "/" + p.Architecture
	if p.OS == "" || p.Architecture == "" || p.Variant != "" || p.OSVersion != "" || len(p.OSFeatures) != 0 {
		return "", fmt.Errorf("unsupported plugin platform %s", key)
	}
	return key, nil
}

type fetcher struct {
	store *Store
	image string
	repo  oras.ReadOnlyTarget
}

func (f *fetcher) metadata(ctx context.Context, desc ocispec.Descriptor, value any) error {
	if _, err := reference(desc, maxMetadataSize); err != nil {
		return err
	}
	data, err := content.FetchAll(ctx, f, desc)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (f *fetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	ref, err := reference(desc, cas.MaxObjectSize)
	if err != nil {
		return nil, err
	}
	if err := f.store.cache.Verify(ctx, ref); err != nil {
		if f.store.offline && len(desc.Data) == 0 {
			return nil, fmt.Errorf("plugin cache unavailable offline: %w", err)
		}
		var blob io.ReadCloser
		if len(desc.Data) != 0 {
			blob = io.NopCloser(bytes.NewReader(desc.Data))
		} else {
			if f.repo == nil {
				f.repo, err = f.store.registry(f.image)
				if err != nil {
					return nil, err
				}
			}
			blob, err = f.repo.Fetch(ctx, desc)
			if err != nil {
				return nil, err
			}
		}
		reader := content.NewVerifyReader(blob, desc)
		_, importErr := f.store.cache.Import(ctx, reader, ref.SHA256)
		verifyErr := reader.Verify()
		closeErr := blob.Close()
		if err := errors.Join(importErr, verifyErr, closeErr); err != nil {
			return nil, err
		}
	}
	path, err := f.store.cache.Path(ref)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func reference(desc ocispec.Descriptor, limit int64) (cas.Ref, error) {
	if err := desc.Digest.Validate(); err != nil || desc.Digest.Algorithm() != digest.SHA256 || desc.Size <= 0 || desc.Size > limit || len(desc.URLs) != 0 {
		return cas.Ref{}, errors.New("invalid plugin content descriptor")
	}
	return cas.Ref{SHA256: desc.Digest.Encoded(), Size: desc.Size}, nil
}
