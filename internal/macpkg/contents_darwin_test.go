package macpkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/sys/unix"
)

func TestBuildRefusesNativeSignatureAttributes(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "helper")
	if err := os.WriteFile(name, []byte("signed content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(name, "com.apple.cs.CodeSignature", []byte("signature"), 0); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Package: Package{Identifier: "org.example.wrapper", Version: "1.0"}, Payload: map[string]Entry{"/Library/Example": {Input: "vendor", Path: "."}}}
	_, err := Build(t.Context(), spec, map[string]plugin.Artifact{"vendor": {Path: root, Tree: true}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "code signature in extended attributes") {
		t.Fatalf("signature discarded: %v", err)
	}
}
