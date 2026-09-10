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

func TestNativeACLRejected(t *testing.T) {
	root, opts := fixture(t)
	name := filepath.Join(root, "Payload/Library/Application Support/Fixture/message.txt")
	native(t, "/bin/chmod", "+a", "everyone allow read", name)
	if err := Build(t.Context(), root, filepath.Join(t.TempDir(), "out.pkg"), opts); err == nil {
		t.Fatal("ACL silently lost")
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

func native(t *testing.T, tool string, args ...string) string {
	t.Helper()
	data, err := exec.CommandContext(t.Context(), tool, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, data)
	}
	return string(data)
}
