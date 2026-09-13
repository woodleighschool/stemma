package macpkg

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deploymenttheory/go-macos-pkg/pkg/cpio"
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
	raw := filepath.Join(t.TempDir(), "raw")
	if output, err := exec.CommandContext(t.Context(), "/usr/sbin/pkgutil", "--expand", artifact.Path, raw).CombinedOutput(); err != nil {
		t.Fatalf("expand archive: %s %v", output, err)
	}
	file, err := os.Open(filepath.Join(raw, "Payload"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	payload, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = payload.Close() }()
	want := map[string][3]uint32{
		"./Library/Fonts":                                {501, 20, 0o755},
		"./Library/Fonts/Example.otf":                    {501, 20, 0o640},
		"./Library/Application Support/Fixture/note.txt": {502, 80, 0},
		"./Library": {0, 0, 0o755},
	}
	reader := cpio.NewReader(payload)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if expected, ok := want[header.Name]; ok {
			if actual := [3]uint32{header.UID, header.GID, header.Mode & 0o777}; actual != expected {
				t.Fatalf("payload metadata for %s: got %v, want %v", header.Name, actual, expected)
			}
			delete(want, header.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing payload entries: %v", want)
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
