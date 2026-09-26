package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestMinimumOSIsDeclaredOrTheLatestRequirement(t *testing.T) {
	installer := func(version string) plugin.Artifact {
		return plugin.Artifact{Facts: plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Kind: "container", Installer: &plugin.InstallerFacts{MinimumOS: version}}}}}
	}
	withApp := func(artifact plugin.Artifact, version string) plugin.Artifact {
		evidence, _ := json.Marshal(plugin.Subject{ID: "Example.app", Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.app", MinimumOS: version}})
		artifact.Evidence = map[string]json.RawMessage{"macos.application": evidence}
		return artifact
	}
	for _, test := range []struct {
		name     string
		artifact plugin.Artifact
		declared string
		want     *plugin.MinimumOS
	}{
		{"installer", withApp(installer("14.2"), "13.0"), "", &plugin.MinimumOS{Version: "14.2", Origin: "installer.minimum_os"}},
		{"application", withApp(installer("13.0"), "14.1"), "", &plugin.MinimumOS{Version: "14.1", Origin: "app.minimum_os"}},
		{"declared above", installer("14.0"), "15", &plugin.MinimumOS{Version: "15", Origin: "software.minimum_os"}},
		{"declared below", withApp(installer("14.0"), "27.0"), "26.0", &plugin.MinimumOS{Version: "26.0", Origin: "software.minimum_os"}},
		{"declared alone", plugin.Artifact{}, "12.0", &plugin.MinimumOS{Version: "12.0", Origin: "software.minimum_os"}},
		{"declared replaces an unreadable requirement", withApp(plugin.Artifact{}, "${MACOSX_DEPLOYMENT_TARGET}"), "12.0", &plugin.MinimumOS{Version: "12.0", Origin: "software.minimum_os"}},
		{"unknown", plugin.Artifact{}, "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := minimumOS(test.artifact, test.declared)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("minimumOS = %+v, want %+v", got, test.want)
			}
		})
	}
	if _, err := minimumOS(withApp(plugin.Artifact{}, "${MACOSX_DEPLOYMENT_TARGET}"), ""); err == nil {
		t.Fatal("an unreadable application requirement was ignored")
	}
}

const minimumProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: minimum
spec:
  imports:
    - '*.software.yaml'
  destinations:
    repository:
      operation: munki
      config:
        path: repository
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: example
spec:
  source: {path: Example.app}
  minimum_os: '%s'
  destinations:
    repository: {}
`

func TestDeclaredMinimumOSReusesPreparation(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{
		"Example.app/Contents/Info.plist":    `<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.app</string><key>CFBundleShortVersionString</key><string>1.2</string><key>CFBundleExecutable</key><string>example</string><key>LSMinimumSystemVersion</key><string>13.0</string></dict></plist>`,
		"Example.app/Contents/MacOS/example": "#!/bin/sh\nexit 0\n",
	} {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	filename := filepath.Join(root, "stemma.yaml")
	var received *plugin.MinimumOS
	record := func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		received = request.MinimumOS
		return plugin.ReconcileResponse{}, nil
	}
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "prepare", Handlers: map[string]reconcileHandler{"munki": record}}
	testproject.Write(t, filename, fmt.Sprintf(minimumProject, "12.0"))
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "plan"
	for _, test := range []struct {
		declared string
		want     plugin.MinimumOS
	}{
		{"12.0", plugin.MinimumOS{Version: "12.0", Origin: "software.minimum_os"}},
		{"15.0", plugin.MinimumOS{Version: "15.0", Origin: "software.minimum_os"}},
	} {
		testproject.Write(t, filename, fmt.Sprintf(minimumProject, test.declared))
		report, err := Run(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Resources[0].Cached {
			t.Fatal("changing minimum_os invalidated preparation")
		}
		if received == nil || *received != test.want {
			t.Fatalf("minimum_os %s sent %+v, want %+v", test.declared, received, test.want)
		}
	}
}
