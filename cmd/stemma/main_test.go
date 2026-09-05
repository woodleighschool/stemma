package main

import (
	"encoding/json"
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

	"github.com/woodleighschool/stemma/internal/engine"
)

func TestCompiledProjectLifecycle(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "stemma")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	installer, err := os.ReadFile("../../internal/apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { downloads.Add(1); _, _ = w.Write(installer) }))
	defer server.Close()
	project := t.TempDir()
	cache := t.TempDir()
	manifest := fmt.Sprintf(`version: 1
project: test-apps
recipes:
  fixture:
    source: {type: http, url: %s/fixture.pkg}
    verification: {integrity: true}
    destinations:
      first: {description: original, unattended_install: false, catalogs: [testing]}
      second: {catalogs: [testing]}
destinations:
  first: {type: munki, path: first}
  second: {type: munki, path: second}
`, server.URL)
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(project, "stemma.yaml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(manifest)
	run := func(success bool, args ...string) engine.Report {
		t.Helper()
		arguments := append([]string{"--root", project, "--cache-dir", cache, "--output", "json"}, args...)
		cmd := exec.CommandContext(t.Context(), binary, arguments...)
		cmd.Env = append(os.Environ(), "CI=true")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if (err == nil) != success {
			t.Fatalf("%v: err=%v stderr=%s output=%s", args, err, stderr.String(), output)
		}
		var report engine.Report
		if len(output) > 0 {
			if err := json.Unmarshal(output, &report); err != nil {
				t.Fatalf("decode %s: %v", output, err)
			}
		}
		return report
	}
	run(false, "prepare")
	run(true, "update")
	if downloads.Load() != 1 {
		t.Fatal("unexpected acquisition count")
	}
	plan := run(true, "plan")
	if len(plan.Recipes) != 1 || len(plan.Recipes[0].Destinations) != 2 {
		t.Fatalf("incomplete plan: %#v", plan)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := os.Stat(filepath.Join(project, name)); !os.IsNotExist(err) {
			t.Fatal("plan mutated destination")
		}
	}
	applied := run(true, "apply")
	for _, destination := range applied.Recipes[0].Destinations {
		if !destination.Applied {
			t.Fatal("destination not applied")
		}
	}
	warm := run(true, "apply", "--offline")
	if !warm.Recipes[0].Prepared.Cached || downloads.Load() != 1 {
		t.Fatal("warm run repeated preparation or acquisition")
	}
	for _, destination := range warm.Recipes[0].Destinations {
		if len(destination.Changes) != 0 {
			t.Fatalf("unchanged run made changes: %#v", destination.Changes)
		}
	}
	write(strings.Replace(manifest, "description: original", "description: edited", 1))
	metadata := run(true, "apply")
	if !metadata.Recipes[0].Prepared.Cached || downloads.Load() != 1 {
		t.Fatal("metadata change invalidated preparation")
	}
	for _, destination := range metadata.Recipes[0].Destinations {
		for _, change := range destination.Changes {
			if change.Kind == "content" {
				t.Fatal("metadata edit uploaded content")
			}
		}
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	cold := run(true, "apply")
	for _, destination := range cold.Recipes[0].Destinations {
		if len(destination.Changes) != 0 {
			t.Fatalf("cold cache replayed publication: %#v", destination.Changes)
		}
	}
	if downloads.Load() != 2 {
		t.Fatal("cold cache did not reacquire once")
	}
	if err := os.RemoveAll(filepath.Join(project, ".stemma", "state")); err != nil {
		t.Fatal(err)
	}
	recovered := run(true, "apply")
	for _, destination := range recovered.Recipes[0].Destinations {
		if len(destination.Changes) != 0 {
			t.Fatal("lost bindings duplicated/replayed destination")
		}
	}
}
