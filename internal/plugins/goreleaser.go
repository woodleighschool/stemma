package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// GoReleaserBundles reads the tar.zst archives GoReleaser recorded in
// dist/artifacts.json as plugin bundles, one for each target it built.
func GoReleaserBundles(dist string) ([]PlatformBundle, error) {
	data, err := os.ReadFile(filepath.Join(dist, "artifacts.json"))
	if err != nil {
		return nil, fmt.Errorf("goreleaser: %w", err)
	}
	var artifacts []struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		GOOS   string `json:"goos"`
		GOARCH string `json:"goarch"`
		Extra  struct {
			Format string `json:"Format"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return nil, fmt.Errorf("goreleaser: artifacts.json: %w", err)
	}
	var bundles []PlatformBundle
	for _, artifact := range artifacts {
		if artifact.Type != "Archive" || artifact.Extra.Format != "tar.zst" {
			continue
		}
		// Universal binaries record the architecture "all", which no runner has.
		if artifact.GOOS == "" || artifact.GOARCH == "" || artifact.GOARCH == "all" {
			return nil, fmt.Errorf("goreleaser: archive %s has no single target platform", artifact.Name)
		}
		// GoReleaser records paths from the directory it ran in, but always
		// writes archives at the top of dist.
		if !filepath.IsLocal(artifact.Name) || filepath.Base(artifact.Name) != artifact.Name {
			return nil, fmt.Errorf("goreleaser: unexpected archive name %q", artifact.Name)
		}
		bundles = append(bundles, PlatformBundle{
			Platform: ocispec.Platform{OS: artifact.GOOS, Architecture: artifact.GOARCH},
			Path:     filepath.Join(dist, artifact.Name),
		})
	}
	if len(bundles) == 0 {
		return nil, errors.New("goreleaser: artifacts.json lists no tar.zst archives")
	}
	return bundles, nil
}
