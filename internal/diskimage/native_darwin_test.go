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

// TestNativeMountReadsPOSIXNames covers bundle names macOS allows and Windows
// does not. GarageBand and iMovie ship them, so the image must carry them
// unchanged rather than reject or rewrite the bundle.
func TestNativeMountReadsPOSIXNames(t *testing.T) {
	app := filepath.Join(t.TempDir(), "NamedFixture.app")
	if err := os.CopyFS(app, os.DirFS("../apple/testdata/NestedFixture.app")); err != nil {
		t.Fatal(err)
	}
	names := []string{"Chasing Shadows Clap:Snare 01.loopdata", `1\16 Alternating Pan.pst`, "trailing "}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(app, "Contents", "Resources", name), []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := filepath.Join(t.TempDir(), "NamedFixture.dmg")
	if err := WriteApplication(t.Context(), app, image, imageTime); err != nil {
		t.Fatal(err)
	}
	native(t, "/usr/bin/hdiutil", "verify", image)
	mountpoint := filepath.Join(t.TempDir(), "volume")
	native(t, "/usr/bin/hdiutil", "attach", "-readonly", "-nobrowse", "-noautoopen", "-mountpoint", mountpoint, image)
	t.Cleanup(func() {
		if output, err := exec.CommandContext(context.Background(), "/usr/bin/hdiutil", "detach", "-force", mountpoint).CombinedOutput(); err != nil {
			t.Errorf("detach: %v: %s", err, output)
		}
	})
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(mountpoint, "NamedFixture.app", "Contents", "Resources", name))
		if err != nil || string(data) != "payload" {
			t.Fatalf("macOS reads %q as %q: %v", name, data, err)
		}
	}
}
