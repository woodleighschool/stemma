package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
)

func TestExecutableRegistersPreparationAndReconciliation(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "echo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write([]byte("fixture content"))
	}))
	defer server.Close()
	manifest := fmt.Sprintf(`version: 1
project: operations
plugins:
  provider:
    trusted: true
    platforms:
      %s/%s: {type: file, path: echo}
recipes:
  fixture:
    source: {type: http, url: %s/payload.bin}
    steps:
      - name: first
        operation: echo.inspect
        inputs: {payload: source}
      - name: inspected
        operation: inspect
        inputs: {input: first/payload}
      - name: second
        operation: echo.inspect
        inputs: {finished: inspected/artifact}
    destinations:
      remote: {artifact: second/finished, displayName: original}
destinations:
  remote: {operation: echo.reconcile, config: {}}
`, runtime.GOOS, runtime.GOARCH, server.URL)
	path := filepath.Join(root, "stemma.yaml")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(manifest)
	project, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockfile.Prepare(t.Context(), project, source.New(store, root, false), lockfile.Options{PluginsOnly: true}); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: path, CacheDir: store.Dir, Method: "plan"}
	if _, err := ValidateProject(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	catalog, err := Catalog(t.Context(), opts)
	if err != nil || len(catalog.Operations) != 8 || downloads.Load() != 0 {
		t.Fatalf("catalog acquired recipe or lost operations: %+v %v downloads=%d", catalog, err, downloads.Load())
	}
	first, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Recipes) != 1 || len(first.Recipes[0].Steps) != 3 || first.Recipes[0].Steps[2].Artifacts["finished"].Payload != first.Recipes[0].Prepared.Source.Artifact {
		t.Fatalf("named step outputs were not preserved: %+v", first)
	}
	write(strings.Replace(manifest, "displayName: original", "displayName: edited", 1))
	second, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range second.Recipes[0].Steps {
		if !step.Cached {
			t.Fatalf("metadata edit invalidated step %s", step.Name)
		}
	}
	if downloads.Load() != 1 {
		t.Fatal("warm operation run redownloaded source")
	}
	if err := os.RemoveAll(store.Dir); err != nil {
		t.Fatal(err)
	}
	third, err := Run(t.Context(), opts)
	if err != nil || third.Recipes[0].Steps[2].Artifacts["finished"].Payload != first.Recipes[0].Steps[2].Artifacts["finished"].Payload {
		t.Fatalf("cold operation output changed: %+v %v", third, err)
	}
	// A second provider advertising the same names cannot shadow the first.
	write(strings.Replace(manifest, "recipes:\n", fmt.Sprintf("  other:\n    trusted: true\n    platforms:\n      %s/%s: {type: file, path: echo}\nrecipes:\n", runtime.GOOS, runtime.GOARCH), 1))
	project, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	installFixturePlugin(t, store, root, binary, "changed resource")
	if _, err := lockfile.Prepare(t.Context(), project, source.New(store, root, false), lockfile.Options{PluginsOnly: true}); err != nil {
		t.Fatal(err)
	}
	count := downloads.Load()
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("provider collision: %v", err)
	}
	if downloads.Load() != count {
		t.Fatal("collision discovered after recipe acquisition")
	}
}
