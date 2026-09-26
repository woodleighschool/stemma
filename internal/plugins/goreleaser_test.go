package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// goreleaserArtifact mirrors the artifacts.json entries GoReleaser writes.
type goreleaserArtifact struct {
	Name   string         `json:"name"`
	Path   string         `json:"path"`
	GOOS   string         `json:"goos,omitempty"`
	GOARCH string         `json:"goarch,omitempty"`
	Target string         `json:"target,omitempty"`
	Type   string         `json:"type"`
	Extra  map[string]any `json:"extra,omitempty"`
}

func archiveArtifact(name, goos, goarch, format string) goreleaserArtifact {
	return goreleaserArtifact{
		Name: name, Path: "dist/" + name, GOOS: goos, GOARCH: goarch, Target: goos + "_" + goarch, Type: "Archive",
		Extra: map[string]any{"Format": format, "ID": "plugin", "WrappedIn": ""},
	}
}

// writeDist writes artifacts.json and, for each named file, its bytes.
func writeDist(t *testing.T, artifacts []goreleaserArtifact, files map[string][]byte) string {
	t.Helper()
	dist := t.TempDir()
	data, err := json.Marshal(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "artifacts.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dist, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

func TestGoReleaserBundlesSelectTarZstdArchives(t *testing.T) {
	dist := writeDist(t, []goreleaserArtifact{
		{Name: "metadata.json", Path: "dist/metadata.json", Type: "Metadata"},
		{Name: "plugin", Path: "dist/plugin_linux_arm64_v8.0/plugin", GOOS: "linux", GOARCH: "arm64", Type: "Binary"},
		archiveArtifact("tools_1.0.0_windows_amd64.tar.zst", "windows", "amd64", "tar.zst"),
		archiveArtifact("tools_1.0.0_linux_arm64.tar.zst", "linux", "arm64", "tar.zst"),
		archiveArtifact("tools_1.0.0_linux_arm64.zip", "linux", "arm64", "zip"),
		{Name: "tools_1.0.0_checksums.txt", Path: "dist/tools_1.0.0_checksums.txt", Type: "Checksum"},
	}, nil)
	bundles, err := GoReleaserBundles(dist)
	if err != nil {
		t.Fatal(err)
	}
	want := []PlatformBundle{
		{Platform: ocispec.Platform{OS: "windows", Architecture: "amd64"}, Path: filepath.Join(dist, "tools_1.0.0_windows_amd64.tar.zst")},
		{Platform: ocispec.Platform{OS: "linux", Architecture: "arm64"}, Path: filepath.Join(dist, "tools_1.0.0_linux_arm64.tar.zst")},
	}
	if !reflect.DeepEqual(bundles, want) {
		t.Fatalf("bundles = %+v, want %+v", bundles, want)
	}
}

func TestGoReleaserBundlesRejectUnusableReleases(t *testing.T) {
	for _, test := range []struct {
		name      string
		artifacts []goreleaserArtifact
		want      string
	}{
		{"no tar.zst archives", []goreleaserArtifact{archiveArtifact("tools_linux_amd64.tar.gz", "linux", "amd64", "tar.gz")}, "no tar.zst archives"},
		{"universal binary", []goreleaserArtifact{archiveArtifact("tools_darwin_all.tar.zst", "darwin", "all", "tar.zst")}, "no single target platform"},
		{"escaping name", []goreleaserArtifact{archiveArtifact("../tools_linux_amd64.tar.zst", "linux", "amd64", "tar.zst")}, "unexpected archive name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := GoReleaserBundles(writeDist(t, test.artifacts, nil))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	t.Run("missing artifacts.json", func(t *testing.T) {
		if _, err := GoReleaserBundles(t.TempDir()); err == nil {
			t.Fatal("dist without artifacts.json accepted")
		}
	})
}
