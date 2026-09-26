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

// artifactFixture mirrors the artifacts.json entries GoReleaser writes.
type artifactFixture struct {
	Name   string         `json:"name"`
	Path   string         `json:"path"`
	GOOS   string         `json:"goos,omitempty"`
	GOARCH string         `json:"goarch,omitempty"`
	Target string         `json:"target,omitempty"`
	Type   string         `json:"type"`
	Extra  map[string]any `json:"extra,omitempty"`
}

func archiveArtifact(id, name, goos, goarch, format string) artifactFixture {
	return artifactFixture{
		Name: name, Path: "dist/" + name, GOOS: goos, GOARCH: goarch, Target: goos + "_" + goarch, Type: "Archive",
		Extra: map[string]any{"Format": format, "ID": id, "WrappedIn": ""},
	}
}

// goreleaserProject writes a project whose dist holds artifacts.json and each
// named file, then runs the rest of the test in the project, where GoReleaser
// records paths from.
func goreleaserProject(t *testing.T, artifacts []artifactFixture, files map[string][]byte) string {
	t.Helper()
	project := t.TempDir()
	dist := filepath.Join(project, "dist")
	if err := os.Mkdir(dist, 0o700); err != nil {
		t.Fatal(err)
	}
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
	t.Chdir(project)
	return project
}

func TestGoReleaserBundlesSelectThePluginArchives(t *testing.T) {
	project := goreleaserProject(t, []artifactFixture{
		{Name: "metadata.json", Path: "dist/metadata.json", Type: "Metadata"},
		{Name: "plugin", Path: "dist/plugin_linux_arm64_v8.0/plugin", GOOS: "linux", GOARCH: "arm64", Type: "Binary", Extra: map[string]any{"ID": "plugin"}},
		archiveArtifact("plugin", "tools_1.0.0_windows_amd64.tar.zst", "windows", "amd64", "tar.zst"),
		archiveArtifact("plugin", "tools_1.0.0_linux_arm64.tar.zst", "linux", "arm64", "tar.zst"),
		archiveArtifact("cli", "tools-cli_1.0.0_linux_arm64.zip", "linux", "arm64", "zip"),
		{Name: "tools_1.0.0_checksums.txt", Path: "dist/tools_1.0.0_checksums.txt", Type: "Checksum"},
	}, nil)
	bundles, err := GoReleaserBundles("dist", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []PlatformBundle{
		{Platform: ocispec.Platform{OS: "windows", Architecture: "amd64"}, Path: filepath.Join(project, "dist", "tools_1.0.0_windows_amd64.tar.zst")},
		{Platform: ocispec.Platform{OS: "linux", Architecture: "arm64"}, Path: filepath.Join(project, "dist", "tools_1.0.0_linux_arm64.tar.zst")},
	}
	if !reflect.DeepEqual(bundles, want) {
		t.Fatalf("bundles = %+v, want %+v", bundles, want)
	}
}

func TestGoReleaserBundlesTakeTheNamedArchiveID(t *testing.T) {
	goreleaserProject(t, []artifactFixture{
		archiveArtifact("plugin", "tools_1.0.0_linux_amd64.tar.zst", "linux", "amd64", "tar.zst"),
		archiveArtifact("debug", "tools-debug_1.0.0_linux_amd64.tar.zst", "linux", "amd64", "tar.zst"),
	}, nil)
	if _, err := GoReleaserBundles("dist", ""); err == nil || !strings.Contains(err.Error(), "--goreleaser-id") {
		t.Fatalf("two tar.zst archive ids: error = %v", err)
	}
	bundles, err := GoReleaserBundles("dist", "plugin")
	if err != nil || len(bundles) != 1 || filepath.Base(bundles[0].Path) != "tools_1.0.0_linux_amd64.tar.zst" {
		t.Fatalf("bundles = %+v, %v", bundles, err)
	}
}

func TestGoReleaserBundlesRejectUnusableReleases(t *testing.T) {
	linux := archiveArtifact("plugin", "tools_linux_amd64.tar.zst", "linux", "amd64", "tar.zst")
	for _, test := range []struct {
		name      string
		artifacts []artifactFixture
		id        string
		want      string
	}{
		{"no tar.zst archives", []artifactFixture{archiveArtifact("plugin", "tools_linux_amd64.tar.gz", "linux", "amd64", "tar.gz")}, "", "no tar.zst archives"},
		{"unknown archive id", []artifactFixture{linux}, "debug", `no archives with id "debug"`},
		{"format override", []artifactFixture{linux, archiveArtifact("plugin", "tools_windows_amd64.zip", "windows", "amd64", "zip")}, "", "is zip, not tar.zst"},
		{"universal binary", []artifactFixture{archiveArtifact("plugin", "tools_darwin_all.tar.zst", "darwin", "all", "tar.zst")}, "", "no single target platform"},
	} {
		t.Run(test.name, func(t *testing.T) {
			goreleaserProject(t, test.artifacts, nil)
			_, err := GoReleaserBundles("dist", test.id)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	t.Run("run outside the project", func(t *testing.T) {
		project := goreleaserProject(t, []artifactFixture{linux}, nil)
		t.Chdir(filepath.Dir(project))
		_, err := GoReleaserBundles(filepath.Join(filepath.Base(project), "dist"), "")
		if err == nil || !strings.Contains(err.Error(), "run publish where GoReleaser ran") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing artifacts.json", func(t *testing.T) {
		if _, err := GoReleaserBundles(t.TempDir(), ""); err == nil {
			t.Fatal("dist without artifacts.json accepted")
		}
	})
}

func TestGoReleaserAnnotationsNameTheBuild(t *testing.T) {
	metadata := []byte(`{"project_name":"tools","tag":"1.2.0","version":"1.2.0","commit":"58d5d19c08d2cbf5cdca2bfd2e658e5f29a130e5","date":"2026-09-26T17:45:36+10:00"}`)
	goreleaserProject(t, nil, map[string][]byte{"metadata.json": metadata})
	annotations, err := GoReleaserAnnotations("dist")
	want := map[string]string{ocispec.AnnotationVersion: "1.2.0", ocispec.AnnotationRevision: "58d5d19c08d2cbf5cdca2bfd2e658e5f29a130e5"}
	if err != nil || !reflect.DeepEqual(annotations, want) {
		t.Fatalf("annotations = %v, %v", annotations, err)
	}
}
