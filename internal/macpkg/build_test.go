package macpkg

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-macos-pkg/pkg/xar"
	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
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
		Scripts: map[string]Script{"postinstall": {Input: "script"}},
	}
	return spec, map[string]plugin.Artifact{"fonts": {Path: filepath.Join(root, "fonts"), Tree: true}, "script": {Path: filepath.Join(root, "postinstall")}}
}

func TestBuildMappedPayloadIsReproducibleAndScriptsAreNotRun(t *testing.T) {
	spec, inputs := fixture(t)
	first, err := buildPackage(t.Context(), spec, inputs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildPackage(t.Context(), spec, inputs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || first.Size != second.Size || first.Version != "1.2.3" {
		t.Fatalf("outputs differ: %+v %+v", first, second)
	}
	if _, err := apple.VerifyPackage(t.Context(), first.Path, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("built package claimed a signer: %v", err)
	}
	facts, err := apple.InspectPackageContents(t.Context(), first.Path)
	if err != nil || len(facts.Packages) != 1 || facts.Packages[0].Identifier != spec.Package.Identifier {
		t.Fatalf("receipt=%+v: %v", facts, err)
	}
	if data, err := os.ReadFile(inputs["script"].Path); err != nil || string(data) != "#!/bin/sh\nexit 93\n" {
		t.Fatal("source script changed")
	}
}

func TestBuildCompressesThePayloadAsDeclared(t *testing.T) {
	for _, test := range []struct {
		name        string
		compression pkgbuild.Compression
		magic       string
	}{
		{"default", "", "\x1f\x8b"},
		{"gzip", pkgbuild.Gzip, "\x1f\x8b"},
		{"xz", pkgbuild.XZ, "pbzx"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, inputs := fixture(t)
			spec.Package.Compression = test.compression
			artifact, err := buildPackage(t.Context(), spec, inputs, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			archive, err := xar.OpenFile(artifact.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = archive.Close() }()
			payload, err := archive.Open(archive.Lookup("Payload"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = payload.Close() }()
			head := make([]byte, len(test.magic))
			if _, err := io.ReadFull(payload, head); err != nil || string(head) != test.magic {
				t.Fatalf("payload begins %q: %v", head, err)
			}
		})
	}
}

func TestSpecRejectsCompressionItCannotApply(t *testing.T) {
	spec, _ := fixture(t)
	spec.Package.Compression = "zstd"
	if err := spec.Validate(); err == nil {
		t.Fatal("unknown compression accepted")
	}
	spec.Package.Compression, spec.Payload = pkgbuild.XZ, nil
	if err := spec.Validate(); err == nil {
		t.Fatal("compression accepted without a payload")
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
				if err := os.Symlink("../outside", filepath.Join(inputs["fonts"].Path, "linked")); err != nil {
					t.Skip(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			work := t.TempDir()
			if _, err := buildPackage(ctx, spec, inputs, work); err == nil {
				t.Fatal("invalid layout accepted")
			}
			files, err := os.ReadDir(work)
			if err != nil || len(files) != 0 {
				t.Fatalf("failed build left files: %v %v", files, err)
			}
		})
	}
}

// buildPackage assembles a package from leased inputs the way preparation does.
func buildPackage(ctx context.Context, spec Spec, inputs map[string]plugin.Artifact, workspace string) (plugin.Artifact, error) {
	sources := newSources(inputs, workspace)
	defer sources.close()
	return build(ctx, spec, sources, workspace)
}
