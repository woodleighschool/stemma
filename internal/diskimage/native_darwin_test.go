package diskimage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestNativeMountVerifiesSignedApplication uses macOS as the oracle: the image
// verifies and mounts, and the signed bundle verifies in place.
func TestNativeMountVerifiesSignedApplication(t *testing.T) {
	app := filepath.Join(t.TempDir(), "NestedFixture.app")
	if err := os.CopyFS(app, os.DirFS("../apple/testdata/NestedFixture.app")); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(t.TempDir(), "NestedFixture.dmg")
	if err := WriteApplication(t.Context(), app, image, imageTime); err != nil {
		t.Fatal(err)
	}
	native(t, "/usr/bin/hdiutil", "verify", image)
	mountpoint := filepath.Join(t.TempDir(), "volume")
	native(t, "/usr/bin/hdiutil", "attach", "-readonly", "-nobrowse", "-noautoopen", "-mountpoint", mountpoint, image)
	t.Cleanup(func() {
		// The test context is cancelled before cleanup runs.
		if output, err := exec.CommandContext(context.Background(), "/usr/bin/hdiutil", "detach", "-force", mountpoint).CombinedOutput(); err != nil {
			t.Errorf("detach: %v: %s", err, output)
		}
	})
	if entries, err := os.ReadDir(mountpoint); err != nil || len(entries) != 1 || entries[0].Name() != "NestedFixture.app" {
		t.Fatalf("mounted volume holds %v: %v", entries, err)
	}
	native(t, "/usr/bin/codesign", "--verify", "--strict", "--deep", filepath.Join(mountpoint, "NestedFixture.app"))
}

func native(t *testing.T, name string, args ...string) {
	t.Helper()
	if output, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", filepath.Base(name), args, err, output)
	}
}
