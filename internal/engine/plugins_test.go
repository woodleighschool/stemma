package engine

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

// TestOnlyUpdatesLockPlugins declares a local plugin: consumers load it from
// its lock entry and never write one, plugins update and stemma update lock
// it, and list reports what each state loads.
func TestOnlyUpdatesLockPlugins(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "local-plugin", "plugin")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: plugins}
spec:
  imports: ['*.software.yaml']
  plugins:
    provider: {path: local-plugin}
---
apiVersion: example.test/v1
kind: ExternalInstaller
metadata: {name: fixture}
spec:
  source: {path: vendor.pkg}
`)
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), []byte("vendor"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "prepare"}
	lockPath := lockfile.Filename(root)

	reports, err := InspectPlugins(t.Context(), opts)
	if !errors.Is(err, ErrPluginsFailed) || len(reports) != 1 || !strings.Contains(reports[0].Error, "not locked") {
		t.Fatalf("unlocked plugin listed as %+v: %v", reports, err)
	}
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "plugin provider did not load") {
		t.Fatalf("prepare with an unlocked plugin: %v", err)
	}
	if locked, err := lockfile.Load(lockPath); err == nil && len(locked.Plugins) != 0 {
		t.Fatalf("prepare locked a plugin: %+v", locked.Plugins)
	}

	update, err := UpdatePlugins(t.Context(), opts, nil)
	if err != nil || !update.LockChanged || len(update.Plugins) != 1 || !update.Plugins[0].Locked || update.Plugins[0].Before != "" {
		t.Fatalf("plugins update = %+v: %v", update, err)
	}
	locked := update.Plugins[0].Digest
	reports, err = InspectPlugins(t.Context(), opts)
	if err != nil || reports[0].Version != "1.0.0" || reports[0].Digest != locked || len(reports[0].Operations) != 3 {
		t.Fatalf("locked plugin listed as %+v: %v", reports, err)
	}
	for _, method := range []string{"update", "prepare"} {
		opts.Method = method
		if _, err := Run(t.Context(), opts); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "local-plugin", "notes.txt"), []byte("rebuilt"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewed, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "changed since they were locked") {
		t.Fatalf("prepare with changed plugin files: %v", err)
	}
	if current, err := os.ReadFile(lockPath); err != nil || !bytes.Equal(current, reviewed) {
		t.Fatalf("prepare changed the lockfile: %v", err)
	}
	opts.Method = "update"
	report, err := Run(t.Context(), opts)
	if err != nil || len(report.Plugins) != 1 || report.Plugins[0].Before.Digest != locked || report.Plugins[0].After.Digest == locked {
		t.Fatalf("stemma update did not relock changed plugin files: %+v %v", report.Plugins, err)
	}

	file, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	file.Plugins["retired"] = plugins.Entry{Declaration: "retired", Digest: locked}
	if err := lockfile.Save(root, file); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEMMA_ECHO_STALE_KIND", "reconcile")
	update, err = UpdatePlugins(t.Context(), opts, []string{"provider"})
	if !errors.Is(err, ErrPluginsFailed) || len(update.Removed) != 1 || update.Removed[0] != "retired" || len(update.Plugins[0].Unavailable) != 1 || update.Plugins[0].Before != update.Plugins[0].Digest {
		t.Fatalf("plugins update of a partly stale plugin = %+v: %v", update, err)
	}
	if _, err := UpdatePlugins(t.Context(), opts, []string{"missing"}); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("update of an undeclared plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.software.yaml"), []byte("invalid: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func() error{
		func() error { _, err := InspectPlugins(t.Context(), opts); return err },
		func() error { _, err := UpdatePlugins(t.Context(), opts, nil); return err },
	} {
		if err := run(); !errors.Is(err, ErrPluginsFailed) {
			t.Fatalf("resource parse error hid the plugin report: %v", err)
		}
	}
}

func TestPluginKindsCannotCollideAcrossInterfaceVersions(t *testing.T) {
	kind := plugin.ResourceKind{APIVersion: "example.test/v1", Kind: "Installer"}
	available := loadedPlugin{name: "current", description: plugin.Description{Descriptor: plugin.Descriptor{Operations: []plugin.Operation{
		{Name: "current.prepare", Kind: "resource", Resource: &kind, SideEffects: "workspace", Methods: []string{"discover", "run"}, InputSchema: []byte(`{}`), OutputSchema: []byte(`{}`)},
	}}}}
	unavailable := loadedPlugin{name: "future", description: plugin.Description{Unavailable: []plugin.Unavailable{
		{Name: "future.prepare", Kind: "resource", Resource: &kind, Reason: "incompatible interface"},
	}}}
	for _, order := range [][]loadedPlugin{{available, unavailable}, {unavailable, available}} {
		t.Run(order[0].name, func(t *testing.T) {
			ops, err := builtins(nil)
			if err != nil {
				t.Fatal(err)
			}
			if !ops.add(order[0]) {
				t.Fatal(ops.failed[order[0].name])
			}
			if ops.add(order[1]) || !strings.Contains(ops.failed[order[1].name].Error(), "resource kind example.test/v1/Installer is already registered") {
				t.Fatalf("colliding kind loaded: %v", ops.failed)
			}
		})
	}
}

func TestPluginInventoryDoesNotAcquireOrExecute(t *testing.T) {
	root := t.TempDir()
	testproject.Write(t, filepath.Join(root, "stemma.yaml"), `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: inventory}
spec:
  imports: ["*.software.yaml"]
  plugins:
    missing: {path: does-not-exist}
    remote: {image: example.invalid/plugin:latest}
`)
	cache := filepath.Join(root, "cache")
	reports, err := ListPlugins(t.Context(), filepath.Join(root, "stemma.yaml"))
	if err != nil || len(reports) != 2 {
		t.Fatalf("inventory: %+v %v", reports, err)
	}
	for _, report := range reports {
		if report.Status != "unlocked" || report.Error != "" || len(report.Operations) != 0 {
			t.Fatalf("invented inspection: %+v", report)
		}
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatal("inventory opened cache", err)
	}
}
