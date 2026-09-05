package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"go.yaml.in/yaml/v4"
)

func TestLocalPackageSharedAcrossDestinations(t *testing.T) {
	root := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stemma.yaml", `version: 1
project: local-packages
imports: [software/**/stemma.yaml]
destinations:
  first: {type: munki, path: first}
  second: {type: munki, path: second}
`)
	fragment := `version: 1
recipes:
  branding:
    source:
      type: local
      include: [Payload/**, Scripts/**]
    verification: {integrity: true}
    artifacts:
      package:
        type: pkg
        identifier: au.edu.vic.woodleigh.fixture
        version: "1.0"
        payload: Payload
        scripts: {postinstall: Scripts/postinstall}
    destinations:
      first: {artifact: package, description: original, catalogs: [testing]}
      second: {artifact: package, catalogs: [testing]}
`
	write("software/Branding/stemma.yaml", fragment)
	write("software/Branding/Payload/Library/Example/message.txt", "payload")
	script := "#!/bin/sh\nexit 0\n"
	write("software/Branding/Scripts/postinstall", script)
	options := Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: t.TempDir(), Method: "apply"}
	run := func() Report {
		t.Helper()
		report, err := Run(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	first := run()
	original := first.Recipes[0].Artifacts["package"]
	if original.Payload.SHA256 == "" || original.Cached || len(first.Recipes[0].Destinations) != 2 {
		t.Fatalf("first build: %+v", first)
	}
	for _, dest := range []string{"first", "second"} {
		packages, err := filepath.Glob(filepath.Join(root, dest, "pkgs", "stemma", original.Payload.SHA256, "*.pkg"))
		if err != nil || len(packages) != 1 {
			t.Fatalf("destination %s did not receive shared package: %v", dest, err)
		}
	}
	options.Lock = lockfile.Options{Frozen: true}
	second := run()
	if !second.Recipes[0].Artifacts["package"].Cached || len(second.Recipes[0].Destinations[0].Changes) != 0 {
		t.Fatalf("unchanged run rebuilt or republished: %+v", second)
	}
	write("software/Branding/Scripts/postinstall", script+"# changed\n")
	if _, err := Run(t.Context(), options); err == nil {
		t.Fatal("frozen execution ignored edited local script")
	}
	options.Lock = lockfile.Options{}
	changed := run()
	current := changed.Recipes[0].Artifacts["package"]
	if current.Payload == original.Payload || current.Cached || !changed.LockChanged {
		t.Fatalf("script edit did not invalidate source and package: %+v", changed)
	}
	write("software/Branding/stemma.yaml", strings.Replace(fragment, "description: original", "description: edited", 1))
	metadata := run()
	if !metadata.Recipes[0].Artifacts["package"].Cached || metadata.LockChanged {
		t.Fatalf("metadata edit invalidated preparation: %+v", metadata)
	}
	for _, change := range metadata.Recipes[0].Destinations[0].Changes {
		if change.Kind == "content" {
			t.Fatal("metadata edit uploaded content")
		}
	}
	if err := os.RemoveAll(options.CacheDir); err != nil {
		t.Fatal(err)
	}
	cold := run()
	if cold.Recipes[0].Artifacts["package"].Payload != current.Payload || len(cold.Recipes[0].Destinations[0].Changes) != 0 {
		t.Fatalf("cold rebuild changed deterministic package or destination: %+v", cold)
	}
	lockPath := filepath.Join(root, "stemma.lock.yaml")
	locked, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := locked.Recipes["branding"]
	entry.ResolvedAt = entry.ResolvedAt.Add(-24 * time.Hour)
	locked.Recipes["branding"] = entry
	lockData, err := yaml.Marshal(locked)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, lockData, 0o600); err != nil {
		t.Fatal(err)
	}
	retimed := run()
	if retimed.Recipes[0].Artifacts["package"].Payload == current.Payload || retimed.Recipes[0].Artifacts["package"].Cached {
		t.Fatal("changed locked package timestamp reused cached package bytes")
	}
	broken := strings.Replace(fragment, "    destinations:\n", `      broken:
        type: pkg
        identifier: au.edu.vic.woodleigh.broken
        version: "1.0"
        scripts: {postinstall: Scripts/missing}
    destinations:
`, 1)
	broken = strings.Replace(broken, "description: original", "description: independent", 1)
	broken = strings.Replace(broken, "second: {artifact: package", "second: {artifact: broken", 1)
	write("software/Branding/stemma.yaml", broken)
	partial, err := Run(t.Context(), options)
	if err == nil || !partial.Recipes[0].Destinations[0].Applied || partial.Recipes[0].Destinations[1].Error == "" {
		t.Fatalf("artifact failure blocked unrelated destination: %+v, err=%v", partial, err)
	}
	write("software/Branding/stemma.yaml", strings.Replace(broken, "second: {artifact: broken", "second: {artifact: package", 1))
	unused := run()
	if _, built := unused.Recipes[0].Artifacts["broken"]; built {
		t.Fatal("publication built an unused artifact")
	}
}
