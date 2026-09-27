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
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/oras-project/oras-go/v3"
	"github.com/oras-project/oras-go/v3/content"
	"github.com/oras-project/oras-go/v3/registry/remote"
	"github.com/oras-project/oras-go/v3/registry/remote/auth"
	"github.com/oras-project/oras-go/v3/registry/remote/credentials"
	"github.com/oras-project/oras-go/v3/registry/remote/properties"
	"github.com/oras-project/oras-go/v3/registry/remote/retry"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/source"
)

const ArtifactType = "application/vnd.stemma.plugin.v1"
const BundleType = "application/vnd.stemma.plugin.bundle.v1.tar+zstd"
const maxMetadataSize = 1 << 20
const maxPlatforms = 64

var zstdMagic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}

// Entry records what locking a plugin learned beyond its declaration: the
// platform index digest a tag named, or the content digest of local files.
// Declaration fingerprints the declaration the entry answers, so an edited
// declaration is never matched with an old answer. An image that names its
// digest needs no entry.
type Entry struct {
	Declaration string `json:"declaration" yaml:"declaration"`
	Digest      string `json:"digest" yaml:"digest"`
}

// Bundle identifies the entire selected package, including executable
// resources. Platforms lists the runners an image has bundles for.
type Bundle struct {
	Manifest   string
	Artifact   cas.Ref
	Local      *source.Content
	Entrypoint string
	Platforms  []string
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
	ref, err := properties.NewReference(image)
	if err != nil {
		return nil, err
	}
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, err
	}
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, err
	}
	client := *retry.DefaultClient
	client.Timeout = 15 * time.Minute
	repo.Registry.Client = &auth.Client{
		Client:         &client,
		Cache:          auth.NewCache(),
		CredentialFunc: remote.NewCredentialFunc(store),
		// A docker login stores a username and password for the distribution
		// token flow; ORAS would otherwise send them through an OAuth2
		// password grant, which not every registry accepts.
		TokenFetcher: auth.NewCompositeTokenFetcher(&client, nil, "", true),
	}
	repo.Registry.MaxMetadataBytes = maxMetadataSize
	return repo, nil
}

// Resolve names the platform index an image's tag selects now. Only updates
// contact the registry for it.
func (s *Store) Resolve(ctx context.Context, image string) (string, error) {
	if s.offline {
		return "", errors.New("cannot resolve a plugin image offline")
	}
	repo, err := s.registry(image)
	if err != nil {
		return "", err
	}
	desc, err := repo.Resolve(ctx, image)
	if err != nil {
		return "", err
	}
	if desc.MediaType != ocispec.MediaTypeImageIndex {
		return "", errors.New("plugin image must be an OCI platform index")
	}
	if _, err := reference(desc, maxMetadataSize); err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// Acquire uses the platform index indexDigest names, even if the image's tag
// has moved. It fetches only the runner's manifest, config and bundle; cached
// bytes are verified first.
func (s *Store) Acquire(ctx context.Context, image, indexDigest string) (Bundle, error) {
	fetcher := &fetcher{store: s, image: image}
	desc, err := fetcher.index(ctx, digest.Digest(indexDigest))
	if err != nil {
		return Bundle{}, err
	}
	var index ocispec.Index
	if err := fetcher.metadata(ctx, desc, &index); err != nil {
		return Bundle{}, err
	}
	if index.SchemaVersion != 2 || index.MediaType != ocispec.MediaTypeImageIndex || len(index.Manifests) == 0 || len(index.Manifests) > maxPlatforms {
		return Bundle{}, errors.New("invalid plugin platform index")
	}
	var selected ocispec.Descriptor
	var platforms []string
	for _, desc := range index.Manifests {
		if desc.MediaType != ocispec.MediaTypeImageManifest || desc.Platform == nil {
			return Bundle{}, errors.New("plugin index requires platform-specific manifests")
		}
		key, err := runnerPlatform(*desc.Platform)
		if err != nil {
			return Bundle{}, err
		}
		if slices.Contains(platforms, key) {
			return Bundle{}, fmt.Errorf("plugin index has more than one bundle for %s", key)
		}
		platforms = append(platforms, key)
		if desc.Platform.OS == s.platform.OS && desc.Platform.Architecture == s.platform.Architecture {
			selected = desc
		}
	}
	slices.Sort(platforms)
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
	return Bundle{Manifest: selected.Digest.String(), Artifact: ref, Platforms: platforms}, nil
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

// index describes the platform index d names. A cached copy supplies its
// size; otherwise the registry does.
func (f *fetcher) index(ctx context.Context, d digest.Digest) (ocispec.Descriptor, error) {
	if err := d.Validate(); err != nil || d.Algorithm() != digest.SHA256 {
		return ocispec.Descriptor{}, errors.New("invalid plugin index digest")
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: d}
	if path, err := f.store.cache.Path(cas.Ref{SHA256: d.Encoded()}); err == nil {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() <= maxMetadataSize {
			desc.Size = info.Size()
			if f.store.cache.Verify(ctx, cas.Ref{SHA256: d.Encoded(), Size: desc.Size}) == nil {
				return desc, nil
			}
		}
	}
	if f.store.offline {
		return ocispec.Descriptor{}, errors.New("plugin cache unavailable offline")
	}
	repo, err := f.repository()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	resolved, err := repo.Resolve(ctx, d.String())
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if resolved.Digest != d || resolved.MediaType != ocispec.MediaTypeImageIndex {
		return ocispec.Descriptor{}, errors.New("plugin image must be an OCI platform index")
	}
	return resolved, nil
}

func (f *fetcher) repository() (oras.ReadOnlyTarget, error) {
	if f.repo == nil {
		repo, err := f.store.registry(f.image)
		if err != nil {
			return nil, err
		}
		f.repo = repo
	}
	return f.repo, nil
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
			repo, err := f.repository()
			if err != nil {
				return nil, err
			}
			blob, err = repo.Fetch(ctx, desc)
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
