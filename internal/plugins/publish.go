package plugins

import (
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

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/oras-project/oras-go/v3"
	"github.com/oras-project/oras-go/v3/content/file"
	"github.com/oras-project/oras-go/v3/errdef"
	"github.com/oras-project/oras-go/v3/registry/remote/properties"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
)

// PlatformBundle is the tar.zst bundle built for one runner platform.
type PlatformBundle struct {
	Platform ocispec.Platform
	Path     string
}

// Publish pushes a manifest for each bundle and tags their platform index as
// image, which must name a tag; annotations apply to the index. It checks
// every bundle against the loader's rules before pushing and tags the index
// last, so a failed publication names no partial release. A published tag
// keeps its release: publishing the same bundles again changes nothing, and
// other bundles need a new tag. Registry credentials come from the Docker
// credential store. It returns the digest of the tagged index.
func Publish(ctx context.Context, image string, bundles []PlatformBundle, annotations map[string]string) (string, error) {
	ref, err := properties.NewReference(image)
	if err != nil || ref.Tag == "" || ref.Digest != "" || strings.Contains(image, "://") {
		return "", errors.New("image must be an OCI registry reference with a tag")
	}
	repo, err := repository(image)
	if err != nil {
		return "", err
	}
	index, err := publish(ctx, repo, ref.Tag, bundles, annotations)
	if err != nil {
		return "", err
	}
	return index.Digest.String(), nil
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
	for i, bundle := range bundles {
		key, err := runnerPlatform(bundle.Platform)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		if i > 0 && bundle.Platform.OS == bundles[i-1].Platform.OS && bundle.Platform.Architecture == bundles[i-1].Platform.Architecture {
			return ocispec.Descriptor{}, fmt.Errorf("plugin release has more than one bundle for %s", key)
		}
		if err := checkBundle(ctx, bundle); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("%s bundle %s: %w", key, filepath.Base(bundle.Path), err)
		}
	}
	release, err := file.New("")
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() { _ = release.Close() }()
	if _, err := oras.PushBytes(ctx, release, ocispec.MediaTypeEmptyJSON, ocispec.DescriptorEmptyJSON.Data); err != nil {
		return ocispec.Descriptor{}, err
	}
	// The manifests and index are written here rather than packed by ORAS,
	// which stamps a creation time and would give identical bundles a new
	// digest.
	manifests := make([]ocispec.Descriptor, len(bundles))
	for i, bundle := range bundles {
		layer, err := release.Add(ctx, filepath.Base(bundle.Path), BundleType, bundle.Path)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		data, err := json.Marshal(ocispec.Manifest{
			SchemaVersion: 2,
			MediaType:     ocispec.MediaTypeImageManifest,
			ArtifactType:  ArtifactType,
			Config:        ocispec.DescriptorEmptyJSON,
			Layers:        []ocispec.Descriptor{layer},
		})
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		manifests[i], err = oras.PushBytes(ctx, release, ocispec.MediaTypeImageManifest, data)
		if err != nil {
			return ocispec.Descriptor{}, err
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
	index, err := oras.TagBytes(ctx, release, ocispec.MediaTypeImageIndex, data, tag)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	// Installs pin the digest a tag named, so a moved tag would give later
	// installs of the same version other bytes.
	switch published, err := target.Resolve(ctx, tag); {
	case errors.Is(err, errdef.ErrNotFound):
	case err != nil:
		return ocispec.Descriptor{}, err
	case published.Digest == index.Digest:
		return index, nil
	default:
		return ocispec.Descriptor{}, fmt.Errorf("tag %s already names release %s; publish changed bundles under a new tag", tag, published.Digest)
	}
	// Copy skips content the registry already holds and pushes the tagged
	// index only after everything it references.
	if _, err := oras.Copy(ctx, release, tag, target, tag, oras.DefaultCopyOptions); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push plugin release: %w", err)
	}
	return index, nil
}

// checkBundle reports whether the loader can run a bundle: a Zstandard tar
// archive that extracts with the platform's entrypoint as a regular file at
// its root.
func checkBundle(ctx context.Context, bundle PlatformBundle) error {
	f, err := os.Open(bundle.Path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > cas.MaxObjectSize {
		return errors.New("plugin bundle must be a regular file of at most 16 GiB")
	}
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || magic != zstdMagic {
		return errors.New("plugin bundle is not a Zstandard archive")
	}
	workspace, err := os.MkdirTemp("", "stemma-plugin-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	extracted := filepath.Join(workspace, "bundle")
	if err := archive.Extract(ctx, bundle.Path, extracted); err != nil {
		return err
	}
	_, err = regularFile(extracted, entrypoint(bundle.Platform.OS))
	return err
}
