package pkgbuild

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNativePackagePayloadScriptsAndBOM(t *testing.T) {
	root, opts := fixture(t)
	output := filepath.Join(t.TempDir(), "fixture.pkg")
	if err := Build(t.Context(), root, output, opts); err != nil {
		t.Fatal(err)
	}
	expanded := filepath.Join(t.TempDir(), "expanded")
	native(t, "/usr/sbin/pkgutil", "--expand-full", output, expanded)
	for _, name := range []string{"Payload/Library/Application Support/Fixture/message.txt", "Scripts/preinstall", "Scripts/postinstall"} {
		actual, err := os.ReadFile(filepath.Join(expanded, name))
		if err != nil {
			t.Fatal(err)
		}
		expected, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("expanded bytes differ for %s", name)
		}
	}
	script, err := os.Stat(filepath.Join(expanded, "Scripts/preinstall"))
	if err != nil {
		t.Fatal(err)
	}
	if script.Mode().Perm() != 0o755 {
		t.Fatalf("hook not executable: %v", script.Mode())
	}
	bom := native(t, "/usr/bin/lsbom", filepath.Join(expanded, "Bom"))
	if !strings.Contains(bom, "./Library/Application Support/Fixture/message.txt\t100640\t0/0\t17\t") {
		t.Fatalf("wrong BOM file semantics: %s", bom)
	}
	opts.Payload = ""
	scriptsOnly := filepath.Join(t.TempDir(), "scripts-only.pkg")
	if err := Build(t.Context(), root, scriptsOnly, opts); err != nil {
		t.Fatal(err)
	}
	scriptDir := filepath.Join(t.TempDir(), "expanded")
	native(t, "/usr/sbin/pkgutil", "--expand-full", scriptsOnly, scriptDir)
	if _, err := os.Stat(filepath.Join(scriptDir, "Payload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scripts-only package has payload")
	}
	for _, hook := range []string{"preinstall", "postinstall"} {
		if _, err := os.Stat(filepath.Join(scriptDir, "Scripts", hook)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeLargeAppPayloadAndBOM(t *testing.T) {
	root, opts := largeFixture(t)
	output := filepath.Join(t.TempDir(), "large.pkg")
	if err := Build(t.Context(), root, output, opts); err != nil {
		t.Fatal(err)
	}
	expanded := filepath.Join(t.TempDir(), "expanded")
	native(t, "/usr/sbin/pkgutil", "--expand-full", output, expanded)
	source := filepath.Join(root, "Fixture.app/Contents/MacOS/large")
	extracted := filepath.Join(expanded, "Payload/Contents/MacOS/large")
	if fileDigest(t, source) != fileDigest(t, extracted) {
		t.Fatal("native extraction changed large executable bytes")
	}
	info, err := os.Stat(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 || !info.ModTime().Equal(opts.Timestamp) {
		t.Fatalf("mode %v, modified %v", info.Mode(), info.ModTime())
	}
	checksum := strings.Fields(native(t, "/usr/bin/cksum", source))[0]
	if _, err := strconv.ParseUint(checksum, 10, 32); err != nil {
		t.Fatal(err)
	}
	bom := native(t, "/usr/bin/lsbom", filepath.Join(expanded, "Bom"))
	want := fmt.Sprintf("./Contents/MacOS/large\t100755\t0/0\t%d\t%s", info.Size(), checksum)
	if !strings.Contains(bom, want) {
		t.Fatalf("BOM does not match independently computed large-file size/checksum: %s", bom)
	}
}

func TestNativeWrapperScriptsResources(t *testing.T) {
	root, opts := wrapperFixture(t)
	output := filepath.Join(t.TempDir(), "wrapper.pkg")
	if err := Build(t.Context(), root, output, opts); err != nil {
		t.Fatal(err)
	}
	expanded := filepath.Join(t.TempDir(), "expanded")
	native(t, "/usr/sbin/pkgutil", "--expand-full", output, expanded)
	for name, mode := range map[string]os.FileMode{
		".": 0o750, "Media.dmg": 0o640, "config": 0o710,
		"config/defaults.plist": 0o440, "config/preinstall": 0o750, "postinstall": 0o755,
	} {
		extracted := filepath.Join(expanded, "Scripts", name)
		info, err := os.Stat(extracted)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode || info.Mode().IsRegular() && !info.ModTime().Equal(opts.Timestamp) {
			t.Fatalf("native scripts metadata for %s: mode %v, modified %v", name, info.Mode(), info.ModTime())
		}
		if info.Mode().IsRegular() && fileDigest(t, extracted) != fileDigest(t, filepath.Join(root, "Scripts", name)) {
			t.Fatalf("native extraction changed resource %s", name)
		}
	}
	for name, want := range map[string]string{"defaults.plist": "config/defaults.plist", "preinstall": "config/preinstall"} {
		link, err := os.Readlink(filepath.Join(expanded, "Scripts", name))
		if err != nil || link != want {
			t.Fatalf("native scripts symlink %s: %q %v", name, link, err)
		}
	}
	for _, name := range []string{"Payload", "Bom"} {
		if _, err := os.Stat(filepath.Join(expanded, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("wrapper unexpectedly includes %s", name)
		}
	}
}

func native(t *testing.T, tool string, args ...string) string {
	t.Helper()
	data, err := exec.CommandContext(t.Context(), tool, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, data)
	}
	return string(data)
}

func TestNativeFrameworkLinksAndMultipleBOMLeaves(t *testing.T) {
	root, opts := fixture(t)
	framework := filepath.Join(root, "Payload/Library/Fixture.framework")
	for i := range 600 {
		writeFile(t, filepath.Join(framework, "Versions/A", fmt.Sprintf("file-%03d", i)), []byte("payload"), 0o644)
	}
	if err := os.Symlink("A", filepath.Join(framework, "Versions/Current")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "framework.pkg")
	if err := Build(t.Context(), root, output, opts); err != nil {
		t.Fatal(err)
	}
	expanded := filepath.Join(t.TempDir(), "expanded")
	native(t, "/usr/sbin/pkgutil", "--expand-full", output, expanded)
	target, err := os.Readlink(filepath.Join(expanded, "Payload/Library/Fixture.framework/Versions/Current"))
	if err != nil || target != "A" {
		t.Fatalf("symlink: %q %v", target, err)
	}
	listing := native(t, "/usr/bin/lsbom", filepath.Join(expanded, "Bom"))
	if !strings.Contains(listing, "./Library/Fixture.framework/Versions/A/file-599\t100644") || !strings.Contains(listing, "./Library/Fixture.framework/Versions/Current\t120755") {
		t.Fatalf("native BOM lost a leaf or symlink: %s", listing)
	}
}
