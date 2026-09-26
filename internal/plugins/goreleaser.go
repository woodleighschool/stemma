package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// goreleaserArtifact is the part of a GoReleaser artifacts.json entry that
// identifies a plugin bundle.
type goreleaserArtifact struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Extra  struct {
		ID     string `json:"ID"`
		Format string `json:"Format"`
	} `json:"extra"`
}

// GoReleaserBundles reads the plugin bundles GoReleaser recorded in
// dist/artifacts.json: the archives of one archive id, which must all be
// tar.zst. id names the archive id; when it is empty, the only archive id that
// builds tar.zst is used. GoReleaser records paths from the directory it ran
// in, so relative paths resolve from the current directory.
func GoReleaserBundles(dist, id string) ([]PlatformBundle, error) {
	data, err := os.ReadFile(filepath.Join(dist, "artifacts.json"))
	if err != nil {
		return nil, fmt.Errorf("goreleaser: %w", err)
	}
	var artifacts []goreleaserArtifact
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return nil, fmt.Errorf("goreleaser: artifacts.json: %w", err)
	}
	archives := map[string][]goreleaserArtifact{}
	for _, artifact := range artifacts {
		if artifact.Type == "Archive" {
			archives[artifact.Extra.ID] = append(archives[artifact.Extra.ID], artifact)
		}
	}
	if id == "" {
		var ids []string
		for candidate, family := range archives {
			if slices.ContainsFunc(family, func(a goreleaserArtifact) bool { return a.Extra.Format == "tar.zst" }) {
				ids = append(ids, candidate)
			}
		}
		switch len(ids) {
		case 0:
			return nil, errors.New("goreleaser: artifacts.json lists no tar.zst archives")
		case 1:
			id = ids[0]
		default:
			slices.Sort(ids)
			return nil, fmt.Errorf("goreleaser: archive ids %s all build tar.zst; choose one with --goreleaser-id", strings.Join(ids, ", "))
		}
	}
	if len(archives[id]) == 0 {
		return nil, fmt.Errorf("goreleaser: artifacts.json lists no archives with id %q", id)
	}
	root, err := filepath.Abs(dist)
	if err != nil {
		return nil, fmt.Errorf("goreleaser: %w", err)
	}
	bundles := make([]PlatformBundle, 0, len(archives[id]))
	for _, artifact := range archives[id] {
		if artifact.Extra.Format != "tar.zst" {
			return nil, fmt.Errorf("goreleaser: archive %s is %s, not tar.zst", artifact.Path, artifact.Extra.Format)
		}
		// Universal binaries record the architecture "all", which no runner has.
		if artifact.GOOS == "" || artifact.GOARCH == "" || artifact.GOARCH == "all" {
			return nil, fmt.Errorf("goreleaser: archive %s has no single target platform", artifact.Path)
		}
		path, err := filepath.Abs(artifact.Path)
		if err != nil {
			return nil, fmt.Errorf("goreleaser: %w", err)
		}
		if rel, err := filepath.Rel(root, path); err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("goreleaser: archive %s is outside %s; run publish where GoReleaser ran", artifact.Path, dist)
		}
		bundles = append(bundles, PlatformBundle{
			Platform: ocispec.Platform{OS: artifact.GOOS, Architecture: artifact.GOARCH},
			Path:     path,
		})
	}
	return bundles, nil
}
