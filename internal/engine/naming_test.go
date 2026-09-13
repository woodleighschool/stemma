package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestBuildPackageFilenameOverride(t *testing.T) {
	for _, override := range []string{"", "Deployment.pkg"} {
		t.Run(override, func(t *testing.T) {
			settings := map[string]any{
				"package": map[string]string{"identifier": "org.example.payload", "version": "4.2", "filename": override},
				"payload": map[string]any{"/Library/Example/message.txt": map[string]string{"content": "synthetic payload"}},
			}
			config, _ := json.Marshal(settings)
			request, _ := json.Marshal(plugin.ResourceRequest{Config: config, Identity: plugin.ResourceReference{Kind: "BuildMacPkg", Name: "payload"}, Workspace: t.TempDir()})
			response, err := buildMacPkg(t.Context(), plugin.Request{Method: "run", Input: request})
			if err != nil {
				t.Fatal(err)
			}
			var result plugin.ResourceResult
			if err := json.Unmarshal(response.Output, &result); err != nil {
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
	request, _ := json.Marshal(plugin.ResourceRequest{Config: json.RawMessage(`{}`), Identity: plugin.ResourceReference{Kind: "MacSoftware", Name: "vendor"}, Workspace: t.TempDir(), Inputs: map[string]plugin.Artifact{"source": {Path: inputPath, Filename: "upstream.pkg"}}})
	response, err := macSoftware(t.Context(), plugin.Request{Method: "run", Input: request})
	if err != nil {
		t.Fatal(err)
	}
	var result plugin.ResourceResult
	if err := json.Unmarshal(response.Output, &result); err != nil {
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
