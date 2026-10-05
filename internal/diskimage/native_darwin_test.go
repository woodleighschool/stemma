package diskimage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeMountVerifiesSignedApplication uses macOS as the oracle: each
// compression is the image format hdiutil names, the image verifies and mounts,
// and the signed bundle verifies in place.
func TestNativeMountVerifiesSignedApplication(t *testing.T) {
	for compression, format := range map[Compression]string{LZFSE: "ULFO", Zlib: "UDZO", LZMA: "ULMO"} {
		t.Run(string(compression), func(t *testing.T) {
			app := filepath.Join(t.TempDir(), "NestedFixture.app")
			if err := os.CopyFS(app, os.DirFS("../apple/testdata/NestedFixture.app")); err != nil {
				t.Fatal(err)
			}
			image := filepath.Join(t.TempDir(), "NestedFixture.dmg")
			if err := writeApplication(t.Context(), app, image, compression, imageTime); err != nil {
				t.Fatal(err)
			}
			native(t, "/usr/bin/hdiutil", "verify", image)
			if info := native(t, "/usr/bin/hdiutil", "imageinfo", image); !strings.Contains(info, "\nFormat: "+format+"\n") {
				t.Fatalf("hdiutil does not see a %s image:\n%s", format, info)
			}
			mountpoint := mount(t, image)
			if entries, err := os.ReadDir(mountpoint); err != nil || len(entries) != 1 || entries[0].Name() != "NestedFixture.app" {
				t.Fatalf("mounted volume holds %v: %v", entries, err)
			}
			native(t, "/usr/bin/codesign", "--verify", "--strict", "--deep", filepath.Join(mountpoint, "NestedFixture.app"))
		})
	}
}

// mount attaches image read-only until the test ends and returns its mountpoint.
func mount(t *testing.T, image string) string {
	t.Helper()
	mountpoint := filepath.Join(t.TempDir(), "volume")
	native(t, "/usr/bin/hdiutil", "attach", "-readonly", "-nobrowse", "-noautoopen", "-mountpoint", mountpoint, image)
	t.Cleanup(func() {
		// The test context is cancelled before cleanup runs.
		if output, err := exec.CommandContext(context.Background(), "/usr/bin/hdiutil", "detach", "-force", mountpoint).CombinedOutput(); err != nil {
			t.Errorf("detach: %v: %s", err, output)
		}
	})
	return mountpoint
}

func native(t *testing.T, name string, args ...string) string {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", filepath.Base(name), args, err, output)
	}
	return string(output)
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
	if err := writeApplication(t.Context(), app, image, LZFSE, imageTime); err != nil {
		t.Fatal(err)
	}
	native(t, "/usr/bin/hdiutil", "verify", image)
	mountpoint := mount(t, image)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(mountpoint, "NamedFixture.app", "Contents", "Resources", name))
		if err != nil || string(data) != "payload" {
			t.Fatalf("macOS reads %q as %q: %v", name, data, err)
		}
	}
}
