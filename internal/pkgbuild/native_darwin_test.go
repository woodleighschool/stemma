package pkgbuild

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func native(t *testing.T, tool string, args ...string) string {
	t.Helper()
	data, err := exec.CommandContext(t.Context(), tool, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, data)
	}
	return string(data)
}
