package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/macpkg"
	"github.com/woodleighschool/stemma/internal/macsoftware"

	"github.com/woodleighschool/stemma/plugin"
)

func TestBuildPackageFilenameOverride(t *testing.T) {
	for _, override := range []string{"", "Deployment.pkg"} {
		t.Run(override, func(t *testing.T) {
			spec := macpkg.Spec{
				Package: macpkg.Package{Identifier: "org.example.payload", Version: "4.2", Filename: override},
				Payload: map[string]macpkg.Entry{"/Library/Example/message.txt": {Content: new("synthetic payload")}},
			}
			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			request := plugin.ResourceRequest[json.RawMessage]{Method: "run", Config: data, Identity: plugin.ResourceReference{Kind: "BuildMacPkg", Name: "payload"}, Workspace: t.TempDir()}
			result, err := buildMacPkg(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			want := override
			if want == "" {
				want = "payload-4.2.pkg"
			}
			artifact := result.Artifacts["installer"]
			if artifact.Filename != want || filepath.Base(artifact.Path) != want {
				t.Fatalf("package name = %q at %s, want %q", artifact.Filename, artifact.Path, want)
			}
			if info, err := os.Stat(artifact.Path); err != nil || info.Size() == 0 {
				t.Fatalf("missing named package: %v", err)
			}
		})
	}
}

func TestMacPublicationNameRetainsInput(t *testing.T) {
	inputPath, err := filepath.Abs("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	request := plugin.ResourceRequest[macsoftware.Spec]{Method: "run", Identity: plugin.ResourceReference{Kind: "MacSoftware", Name: "vendor"}, Workspace: t.TempDir(), Inputs: map[string]plugin.Artifact{"source": {Path: inputPath, Filename: "upstream.pkg"}}}
	result, err := macSoftware(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	installer := result.Artifacts["installer"]
	if installer.Filename != "vendor-"+installer.Version+".pkg" || installer.Version == "" {
		t.Fatalf("missing publication name or version: %+v", installer)
	}
	prepared, err := os.ReadFile(installer.Path)
	if err != nil || string(prepared) != string(data) {
		t.Fatalf("naming changed the vendor installer: %v", err)
	}
}
