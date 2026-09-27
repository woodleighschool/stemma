package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/plugins"
)

func TestChangedSinceUsesEachCatalogsPluginOperations(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "echo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	// The next implementation renames a configuration field. Comparing either
	// declaration through the other implementation must fail its schema.
	code, err := os.ReadFile("../../plugin/testdata/echo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	nextSource := filepath.Join(t.TempDir(), "main.go")
	writeFileText(t, nextSource, strings.Replace(string(code), `json:"source"`, `json:"input"`, 1))
	nextBinary := filepath.Join(t.TempDir(), "echo")
	build = exec.CommandContext(t.Context(), "go", "build", "-o", nextBinary, nextSource)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build next plugin: %v\n%s", err, output)
	}
	for _, test := range []struct {
		name, transport, use string
		want                 []string
	}{
		{"resource implementation", "image", "resource", []string{"example.test/v1/ExternalInstaller/external", "MacSoftware/external-consumer"}},
		{"resolver implementation", "image", "resolver", []string{"MacSoftware/external-consumer"}},
		{"destination implementation", "image", "destination", nil},
		{"reviewed tag lock", "tag", "resource", []string{"example.test/v1/ExternalInstaller/external", "MacSoftware/external-consumer"}},
		{"changed resource contract", "contract", "resource", []string{"example.test/v1/ExternalInstaller/external", "MacSoftware/external-consumer"}},
		{"local plugin files", "path", "resource", []string{"example.test/v1/ExternalInstaller/external", "MacSoftware/external-consumer"}},
		{"same digest under another tag", "rename", "resource", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, _ := changedCatalog(t)
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: store.Dir}
			manifest := changedProject
			install := func(revision string) {
				t.Helper()
				image := "registry.example/plugins/echo:v1"
				declaration := "image: " + image
				var digest string
				selected := binary
				if test.transport == "contract" && revision == "current" {
					selected = nextBinary
				}
				if test.transport == "path" || test.transport == "contract" {
					declaration = "path: local-plugin"
					data, err := os.ReadFile(selected)
					if err != nil {
						t.Fatal(err)
					}
					name := "plugin"
					if runtime.GOOS == "windows" {
						name += ".exe"
					}
					path := filepath.Join(root, "local-plugin", name)
					writeFile(t, path, data)
					if err := os.Chmod(path, 0o755); err != nil {
						t.Fatal(err)
					}
					writeFileText(t, filepath.Join(root, "local-plugin", "resource.txt"), revision)
				} else {
					content := revision
					if test.transport == "rename" {
						content = "base"
						declaration = "image: registry.example/plugins/echo:" + revision
					}
					digest = installFixturePlugin(t, store, binary, content)
					if test.transport != "tag" {
						declaration += "@" + digest
					}
				}
				writeFileText(t, opts.ConfigPath, strings.Replace(manifest, "  imports:", "  plugins:\n    provider: {"+declaration+"}\n  imports:", 1))
				if test.transport == "tag" {
					locked, err := lockfile.Load(lockfile.Filename(root))
					if err != nil {
						t.Fatal(err)
					}
					locked.Plugins = map[string]plugins.Entry{"provider": {Declaration: plugins.Fingerprint(config.Plugin{Image: image}), Digest: digest}}
					if err := lockfile.Save(root, locked); err != nil {
						t.Fatal(err)
					}
				}
				if test.transport == "path" || test.transport == "contract" {
					if _, err := UpdatePlugins(t.Context(), opts, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch test.use {
			case "resource":
				writeFileText(t, filepath.Join(root, "software", "external.yaml"), `apiVersion: example.test/v1
kind: ExternalInstaller
metadata: {name: external}
spec:
  source: {resolver: file, path: app.pkg}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: external-consumer}
spec:
  extends: app
  source: {resource: {apiVersion: example.test/v1, kind: ExternalInstaller, name: external, output: installer}}
`)
			case "resolver":
				// Reuse the fixture's real HTTP resolver and reviewed input bytes.
				payload, err := os.ReadFile("../apple/testdata/fixture.pkg")
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) }))
				t.Cleanup(server.Close)
				url := server.URL
				writeFileText(t, filepath.Join(root, "software", "external.yaml"), `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: external-consumer}
spec:
  extends: app
  source: {resolver: echo.download, url: `+url+`/vendor.pkg}
`)
			case "destination":
				manifest = strings.Replace(manifest, "operation: munki, config: {path: repo}", "operation: echo.reconcile", 1)
			}
			install("base")
			opts.Method = "update"
			if _, err := Run(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			commit(t, root)
			opts.Method, opts.ChangedSince = "prepare", "HEAD"
			if report, err := Run(t.Context(), opts); err != nil || len(report.Resources) != 0 {
				t.Fatalf("unchanged plugins prepared %v: %v", preparedKeys(report), err)
			}
			if test.transport == "contract" {
				path := filepath.Join(root, "software", "external.yaml")
				document, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				writeFileText(t, path, strings.Replace(string(document), "source: {resolver: file", "input: {resolver: file", 1))
			}
			install("current")
			if report, err := Run(t.Context(), opts); err != nil || !slices.Equal(preparedKeys(report), test.want) {
				t.Fatalf("changed plugin prepared %v, want %v: %v", preparedKeys(report), test.want, err)
			}
			if _, err := ValidateProject(t.Context(), opts, false); err != nil {
				t.Fatalf("current plugin contracts: %v", err)
			}
			if test.transport == "path" {
				// A missing historical plugin lock cannot be interpreted using
				// current code or silently treated as a preparation change.
				path := lockfile.Filename(root)
				current, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				locked, err := lockfile.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				locked.Plugins = nil
				if err := lockfile.Save(root, locked); err != nil {
					t.Fatal(err)
				}
				commit(t, root)
				writeFile(t, path, current)
				if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "at HEAD") || !strings.Contains(err.Error(), "local files are not locked") {
					t.Fatalf("missing base plugin lock: %v", err)
				}
			}
		})
	}
}
