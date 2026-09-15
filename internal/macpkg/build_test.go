package macpkg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

func fixture(t *testing.T) (Spec, map[string]plugin.Artifact) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "fonts"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"fonts/Example.otf": "synthetic font bytes", "postinstall": "#!/bin/sh\nexit 93\n"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	note := "managed font fixture\n"
	spec := Spec{
		Inputs:  map[string]plugin.Input{"fonts": {}, "script": {}},
		Package: Package{Identifier: "org.example.fonts", Version: "1.2.3", Filename: "Fonts.pkg"},
		Payload: map[string]Entry{
			"/Library/Fonts": {Input: "fonts", Mode: "0755", UID: 501, GID: 20},
			"Library/Application Support/Fixture/note.txt": {Content: &note, Mode: "0000", UID: 502, GID: 80},
		},
		Scripts: map[string]InputFileRef{"postinstall": {Input: "script"}},
	}
	return spec, map[string]plugin.Artifact{"fonts": {Path: filepath.Join(root, "fonts"), Tree: true}, "script": {Path: filepath.Join(root, "postinstall")}}
}

func TestBuildMappedPayloadIsReproducibleAndScriptsAreNotRun(t *testing.T) {
	spec, inputs := fixture(t)
	first, err := Build(t.Context(), spec, inputs, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(t.Context(), spec, inputs, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || first.Size != second.Size || first.Version != "1.2.3" {
		t.Fatalf("outputs differ: %+v %+v", first, second)
	}
	if _, err := apple.VerifyPackage(t.Context(), first.Path, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("built package claimed a signer: %v", err)
	}
	facts, err := apple.InspectPackage(first.Path)
	if err != nil || len(facts.Packages) != 1 || facts.Packages[0].Identifier != spec.Package.Identifier {
		t.Fatalf("receipt=%+v: %v", facts, err)
	}
	if data, err := os.ReadFile(inputs["script"].Path); err != nil || string(data) != "#!/bin/sh\nexit 93\n" {
		t.Fatal("source script changed")
	}
}

func TestBuildRejectsUnsafeLayout(t *testing.T) {
	for _, name := range []string{"traversal", "overlap", "mode", "ownership", "symlink", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			spec, inputs := fixture(t)
			ctx := t.Context()
			switch name {
			case "traversal":
				spec.Payload["../../outside"] = Entry{}
			case "overlap":
				spec.Payload["Library/Fonts/Example.otf"] = Entry{}
			case "mode":
				spec.Payload["private"] = Entry{Mode: "4755"}
			case "ownership":
				spec.Payload["private"] = Entry{UID: 1 << 18}
			case "symlink":
				if err := os.Symlink("Example.otf", filepath.Join(inputs["fonts"].Path, "linked")); err != nil {
					t.Skip(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			work := t.TempDir()
			if _, err := Build(ctx, spec, inputs, work, time.Time{}); err == nil {
				t.Fatal("invalid layout accepted")
			}
			files, err := os.ReadDir(work)
			if err != nil || len(files) != 0 {
				t.Fatalf("failed build left files: %v %v", files, err)
			}
		})
	}
}
