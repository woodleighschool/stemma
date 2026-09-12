package macpkg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeMetadataMatchesPayloadDeclaration(t *testing.T) {
	spec, inputs := fixture(t)
	artifact, err := Build(t.Context(), spec, inputs, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	expanded := filepath.Join(t.TempDir(), "expanded")
	if output, err := exec.CommandContext(t.Context(), "/usr/sbin/pkgutil", "--expand-full", artifact.Path, expanded).CombinedOutput(); err != nil {
		t.Fatalf("expand: %s %v", output, err)
	}
	bom, err := exec.CommandContext(t.Context(), "/usr/bin/lsbom", filepath.Join(expanded, "Bom")).CombinedOutput()
	if err != nil {
		t.Fatalf("lsbom: %s %v", bom, err)
	}
	for _, want := range []string{"./Library/Fonts\t40755\t501/20", "./Library/Fonts/Example.otf\t100640\t501/20", "./Library/Application Support/Fixture/note.txt\t100000\t502/80"} {
		if !strings.Contains(string(bom), want) {
			t.Fatalf("missing %q in BOM: %s", want, bom)
		}
	}
	script, err := os.ReadFile(filepath.Join(expanded, "Scripts", "postinstall"))
	if err != nil || string(script) != "#!/bin/sh\nexit 93\n" {
		t.Fatalf("script changed: %q %v", script, err)
	}
	info, err := os.Stat(filepath.Join(expanded, "Payload", "Library", "Fonts", "Example.otf"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("payload mode differs: %v %v", info, err)
	}
}
