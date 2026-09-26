package plugins

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

// PlatformBundle is the tar.zst bundle built for one runner platform.
type PlatformBundle struct {
	Platform ocispec.Platform
	Path     string
}

// Publish pushes a manifest for each bundle and tags their platform index as
// image, which must name a tag; annotations apply to the index. It checks
// every bundle against the loader's rules before pushing and moves the tag
// last, so a failed publication leaves the previous release in place.
// Registry credentials come from the Docker credential store. The returned
// entry is the pin stemma plugins install records for the tag.
func Publish(ctx context.Context, image string, bundles []PlatformBundle, annotations map[string]string) (Entry, error) {
	ref, err := registry.ParseReference(image)
	if err != nil || ref.ValidateReferenceAsTag() != nil {
		return Entry{}, errors.New("image must be an OCI registry reference with a tag")
	}
	repo, err := repository(image)
	if err != nil {
		return Entry{}, err
	}
	index, err := publish(ctx, repo, ref.Reference, bundles, annotations)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Image: image, Digest: index.Digest.String(), Size: index.Size}, nil
}

func publish(ctx context.Context, target oras.Target, tag string, bundles []PlatformBundle, annotations map[string]string) (ocispec.Descriptor, error) {
	if len(bundles) == 0 || len(bundles) > maxPlatforms {
		return ocispec.Descriptor{}, fmt.Errorf("plugin release needs 1 to %d bundles, not %d", maxPlatforms, len(bundles))
	}
	// Build tools finish targets in any order; sorting keeps the index digest
	// stable for the same bundles.
	bundles = slices.SortedFunc(slices.Values(bundles), func(a, b PlatformBundle) int {
		return cmp.Or(strings.Compare(a.Platform.OS, b.Platform.OS), strings.Compare(a.Platform.Architecture, b.Platform.Architecture))
	})
	layers := make([]ocispec.Descriptor, len(bundles))
	for i, bundle := range bundles {
		key, err := runnerPlatform(bundle.Platform)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		if i > 0 && bundle.Platform.OS == bundles[i-1].Platform.OS && bundle.Platform.Architecture == bundles[i-1].Platform.Architecture {
			return ocispec.Descriptor{}, fmt.Errorf("plugin release has more than one bundle for %s", key)
		}
		layers[i], err = checkBundle(ctx, bundle)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("%s bundle %s: %w", key, filepath.Base(bundle.Path), err)
		}
	}
	if _, err := pushBytes(ctx, target, ocispec.MediaTypeEmptyJSON, ocispec.DescriptorEmptyJSON.Data); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push plugin config: %w", err)
	}
	manifests := make([]ocispec.Descriptor, len(bundles))
	for i, bundle := range bundles {
		if err := pushFile(ctx, target, layers[i], bundle.Path); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("push %s bundle: %w", filepath.Base(bundle.Path), err)
		}
		data, err := json.Marshal(ocispec.Manifest{
			SchemaVersion: 2,
			MediaType:     ocispec.MediaTypeImageManifest,
			ArtifactType:  ArtifactType,
			Config:        ocispec.DescriptorEmptyJSON,
			Layers:        []ocispec.Descriptor{layers[i]},
		})
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		manifests[i], err = pushBytes(ctx, target, ocispec.MediaTypeImageManifest, data)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("push %s manifest: %w", filepath.Base(bundle.Path), err)
		}
		manifests[i].ArtifactType = ArtifactType
		manifests[i].Platform = &bundle.Platform
	}
	data, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests:     manifests,
		Annotations:   annotations,
	})
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if len(data) > maxMetadataSize {
		return ocispec.Descriptor{}, errors.New("plugin index exceeds 1 MiB")
	}
	index, err := oras.TagBytes(ctx, target, ocispec.MediaTypeImageIndex, data, tag)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push plugin index: %w", err)
	}
	return index, nil
}

// checkBundle describes a bundle the loader can run: a Zstandard tar archive
// that extracts with the platform's entrypoint as a regular file at its root.
func checkBundle(ctx context.Context, bundle PlatformBundle) (ocispec.Descriptor, error) {
	f, err := os.Open(bundle.Path)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > cas.MaxObjectSize {
		return ocispec.Descriptor{}, errors.New("plugin bundle must be a regular file of at most 16 GiB")
	}
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || magic != zstdMagic {
		return ocispec.Descriptor{}, errors.New("plugin bundle is not a Zstandard archive")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ocispec.Descriptor{}, err
	}
	sum, err := digest.SHA256.FromReader(f)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	workspace, err := os.MkdirTemp("", "stemma-plugin-")
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	extracted := filepath.Join(workspace, "bundle")
	if err := archive.Extract(ctx, bundle.Path, extracted); err != nil {
		return ocispec.Descriptor{}, err
	}
	if _, err := regularFile(extracted, entrypoint(bundle.Platform.OS)); err != nil {
		return ocispec.Descriptor{}, err
	}
	return ocispec.Descriptor{
		MediaType: BundleType,
		Digest:    sum,
		Size:      info.Size(),
		// ORAS pulls only layers that carry a file name.
		Annotations: map[string]string{ocispec.AnnotationTitle: filepath.Base(bundle.Path)},
	}, nil
}

// pushBytes and pushFile skip content the registry already holds, so a
// repeated publication uploads nothing new.
func pushBytes(ctx context.Context, target oras.Target, mediaType string, data []byte) (ocispec.Descriptor, error) {
	desc := content.NewDescriptorFromBytes(mediaType, data)
	if exists, err := target.Exists(ctx, desc); err != nil || exists {
		return desc, err
	}
	if err := target.Push(ctx, desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, err
	}
	return desc, nil
}

func pushFile(ctx context.Context, target oras.Target, desc ocispec.Descriptor, path string) error {
	if exists, err := target.Exists(ctx, desc); err != nil || exists {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := target.Push(ctx, desc, f); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}
