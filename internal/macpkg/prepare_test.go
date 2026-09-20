package macpkg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/plugin"
)

func prepareApplication(t *testing.T, name string) (string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		name + "/Contents/Info.plist":    `<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.fixture</string><key>CFBundleExecutable</key><string>fixture</string><key>CFBundleShortVersionString</key><string>1.2.3</string><key>CFBundleVersion</key><string>123</string></dict></plist>`,
		name + "/Contents/MacOS/fixture": "#!/bin/sh\nexit 97\n",
		"tenant.json":                    "{\"tenant\":\"synthetic\"}\n",
	}
	for name, data := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, files
}

func prepareRequest(t *testing.T, config map[string]any, inputs map[string]plugin.Artifact) plugin.ResourceRequest[json.RawMessage] {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return plugin.ResourceRequest[json.RawMessage]{Config: data, Identity: plugin.ResourceReference{Name: "Fixture"}, Inputs: inputs, Workspace: t.TempDir()}
}

func TestPrepareInspectsAndCopiesTreeAndZIPWithoutChangingSources(t *testing.T) {
	root, original := prepareApplication(t, "Example.app")
	archive := filepath.Join(t.TempDir(), "vendor.zip")
	testarchive.Zip(t, archive, root)
	archiveBytes, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"tree", "zip"} {
		t.Run(format, func(t *testing.T) {
			input := plugin.Artifact{Path: root, Tree: true, Filename: "vendor"}
			if format == "zip" {
				input = plugin.Artifact{Path: archive, Filename: "vendor.zip", Format: "zip"}
			}
			config := map[string]any{
				"inspect": map[string]any{"app": map[string]any{"$input": "vendor", "path": "Example.app"}},
				"package": map[string]any{"identifier": "org.example.wrapper", "version": "{{ facts.app.app.version + '-r' + env.REVISION }}"},
				"payload": map[string]any{"/Applications/Example.app": map[string]any{"$input": "vendor", "path": "Example.app"}},
				"scripts": map[string]any{
					"postinstall": map[string]any{"content": "#!/bin/sh\nexit 96\n# {{ facts.app.app.version }}\n"},
					"resources":   map[string]any{"$input": "vendor", "path": "Example.app"},
					"tenant.json": map[string]any{"$input": "vendor", "path": "tenant.json"},
				},
			}
			if format == "zip" {
				config["scripts"].(map[string]any)["vendor.zip"] = map[string]any{"$input": "vendor"}
			}
			request := prepareRequest(t, config, map[string]plugin.Artifact{"vendor": input})
			request.Environment = map[string]string{"REVISION": "2"}
			result, err := Prepare(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Version != "1.2.3-r2" || result.Filename != "Fixture-1.2.3-r2.pkg" {
				t.Fatalf("wrong derived package identity: %+v", result)
			}
			receipts, err := apple.InspectPackage(result.Path)
			if err != nil || len(receipts.Packages) != 1 || receipts.Packages[0].Version != result.Version {
				t.Fatalf("wrong resolved receipt: %+v, %v", receipts, err)
			}
			payload, _ := packageArchive(t, result.Path, "Payload")
			scripts, _ := packageArchive(t, result.Path, "Scripts")
			if payload["Applications/Example.app/Contents/Info.plist"] != original["Example.app/Contents/Info.plist"] ||
				scripts["resources/Contents/MacOS/fixture"] != original["Example.app/Contents/MacOS/fixture"] ||
				scripts["tenant.json"] != original["tenant.json"] || scripts["postinstall"] != "#!/bin/sh\nexit 96\n# 1.2.3\n" {
				t.Fatal("inspection or copying changed the app or script resources")
			}
			if format == "zip" && !bytes.Equal([]byte(scripts["vendor.zip"]), archiveBytes) {
				t.Fatal("copying original media changed the archive bytes")
			}
			for name, want := range original {
				got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
				if err != nil || string(got) != want {
					t.Fatalf("source %s changed: %v", name, err)
				}
			}
		})
	}
	current, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(current, archiveBytes) {
		t.Fatalf("source archive changed: %v", err)
	}
}

func TestPrepareValidatesAllInspectionsBeforeOpeningInputs(t *testing.T) {
	for _, test := range []struct {
		name       string
		inspection string
		selection  map[string]any
		want       string
	}{
		{"name", "z/../../escape", map[string]any{"$input": "vendor"}, "invalid inspection name"},
		{"input", "z", map[string]any{"$input": "unknown"}, "unknown input"},
		{"path", "z", map[string]any{"$input": "vendor", "path": "../escape"}, "input path must be confined"},
		{"signature", "z", map[string]any{"$input": "vendor", "signature": map[string]any{"signer": "invalid"}}, "signature"},
		{"signer scheme", "z", map[string]any{"$input": "vendor", "signature": map[string]any{"signer": "authenticode:" + strings.Repeat("a", 64)}}, "Apple Developer ID"},
		{"unknown property", "z", map[string]any{"$input": "vendor", "typo": true}, "unknown field"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]any{
				"inspect": map[string]any{"a": map[string]any{"$input": "vendor"}, test.inspection: test.selection},
				"package": map[string]any{"identifier": "org.example.fixture", "version": "1"},
			}
			request := prepareRequest(t, config, map[string]plugin.Artifact{"vendor": {Path: filepath.Join(t.TempDir(), "does-not-exist")}})
			_, err := Prepare(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("inspection was not validated before opening the input: %v", err)
			}
			files, err := os.ReadDir(request.Workspace)
			if err != nil || len(files) != 0 {
				t.Fatalf("invalid inspection wrote workspace files: %v", err)
			}
		})
	}
}

func TestPrepareDoesNotRenderConcreteInspectionFieldsAgain(t *testing.T) {
	name := "{{ env.REPLACEMENT }}.app"
	root, _ := prepareApplication(t, name)
	config := map[string]any{
		"inspect": map[string]any{"app": map[string]any{"$input": "vendor", "path": name}},
		"package": map[string]any{"identifier": "org.example.fixture", "version": "{{ facts.app.app.version }}"},
		"payload": map[string]any{"/Applications/Example.app": map[string]any{"$input": "vendor", "path": "\\" + name}},
	}
	request := prepareRequest(t, config, map[string]plugin.Artifact{"vendor": {Path: root, Tree: true}})
	request.Environment = map[string]string{"REPLACEMENT": "wrong"}
	result, err := Prepare(t.Context(), request)
	if err != nil || result.Version != "1.2.3" {
		t.Fatalf("concrete inspection path was rendered again: %+v, %v", result, err)
	}
}

func TestPrepareUsesOnlyTheRequestEnvironment(t *testing.T) {
	t.Setenv("STEMMA_PREPARE_VERSION", "process-only-value")
	for _, test := range []struct {
		name        string
		version     string
		environment map[string]string
		want        string
	}{
		{"nil optional", `{{ env.?STEMMA_PREPARE_VERSION.orValue("1") }}`, nil, "1"},
		{"missing strict", "{{ env.STEMMA_PREPARE_VERSION }}", nil, ""},
		{"request wins", "{{ env.STEMMA_PREPARE_VERSION }}", map[string]string{"STEMMA_PREPARE_VERSION": "2"}, "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]any{
				"package": map[string]any{"identifier": "org.example.fixture", "version": test.version},
				"scripts": map[string]any{"postinstall": map[string]any{"content": "#!/bin/sh\nexit 96\n"}},
			}
			request := prepareRequest(t, config, nil)
			request.Environment = test.environment
			result, err := Prepare(t.Context(), request)
			if test.want == "" {
				if err == nil || strings.Contains(err.Error(), "process-only-value") {
					t.Fatalf("missing request environment was not rejected safely: %v", err)
				}
			} else if err != nil || result.Version != test.want {
				t.Fatalf("wrong environment: %+v, %v", result, err)
			}
		})
	}
}

func TestPrepareRejectsWrongResolvedTypesAndMissingFacts(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		script  any
		want    string
	}{
		{"boolean version", "{{ true }}", map[string]any{"content": "literal"}, "resolved package config"},
		{"numeric content", "1", map[string]any{"content": "{{ 7 }}"}, "resolved package config"},
		{"boolean owner", "1", map[string]any{"content": "literal", "uid": "{{ false }}"}, "resolved package config"},
		{"missing fact", "{{ facts.missing.app.version }}", map[string]any{"content": "literal"}, "required reference is missing"},
		{"runner path", "{{ inputs.vendor.path }}", map[string]any{"content": "literal"}, "required reference is missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := prepareRequest(t, map[string]any{
				"package": map[string]any{"identifier": "org.example.fixture", "version": test.version},
				"scripts": map[string]any{"postinstall": test.script},
			}, map[string]plugin.Artifact{"vendor": {Path: "/private/runner/input"}})
			_, err := Prepare(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid resolved config accepted: %v", err)
			}
			files, err := os.ReadDir(request.Workspace)
			if err != nil || len(files) != 0 {
				t.Fatalf("invalid config produced files: %v", err)
			}
		})
	}
}

func TestPrepareTreatsMissingInputEvidenceAsAnEmptyObject(t *testing.T) {
	request := prepareRequest(t, map[string]any{
		"package": map[string]any{"identifier": "org.example.fixture", "version": "1"},
		"scripts": map[string]any{
			"postinstall": map[string]any{"content": "#!/bin/sh\nexit 96\n"},
			"release":     map[string]any{"content": `{{ inputs.vendor.evidence.?release.orValue("unknown") }}`},
			"count":       map[string]any{"content": `{{ string(size(inputs.vendor.evidence)) }}`},
		},
	}, map[string]plugin.Artifact{"vendor": {}})
	result, err := Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := packageArchive(t, result.Path, "Scripts")
	if files["release"] != "unknown" || files["count"] != "0" {
		t.Fatalf("missing evidence did not behave as an empty object: %v", files)
	}
}

func TestPrepareExpressionsCannotIntroduceInputReferences(t *testing.T) {
	for _, area := range []string{"payload", "scripts"} {
		for _, wholeArea := range []bool{false, true} {
			config := map[string]any{"package": map[string]any{"identifier": "org.example.fixture", "version": "1"}}
			if wholeArea {
				config[area] = `{{ {"entry": {"$input": "vendor"}} }}`
			} else {
				config[area] = map[string]any{"entry": `{{ {"$input": "vendor"} }}`}
			}
			request := prepareRequest(t, config, map[string]plugin.Artifact{"vendor": {Path: filepath.Join(t.TempDir(), "not-opened")}})
			_, err := Prepare(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), "$input references must be declared") {
				t.Fatalf("%s parent=%v introduced an input reference: %v", area, wholeArea, err)
			}
		}
	}
	for _, input := range []string{"other", ""} {
		request := prepareRequest(t, map[string]any{
			"package": map[string]any{"identifier": "org.example.fixture", "version": "1"},
			"scripts": map[string]any{"entry": map[string]any{"$input": "{{ inputs.vendor.version }}"}},
		}, map[string]plugin.Artifact{"vendor": {Path: filepath.Join(t.TempDir(), "not-opened"), Version: input}})
		_, err := Prepare(t.Context(), request)
		if err == nil || !strings.Contains(err.Error(), "$input references must be declared") {
			t.Fatalf("expression changed an input reference to %q: %v", input, err)
		}
	}
	request := prepareRequest(t, map[string]any{
		"package": map[string]any{"identifier": "org.example.fixture", "version": "1"},
		"payload": map[string]any{"/Library/Example/note.txt": `{{ {"content": "native map", "mode": "0644"} }}`},
	}, nil)
	result, err := Prepare(t.Context(), request)
	if err != nil {
		t.Fatalf("native data map was rejected: %v", err)
	}
	files, _ := packageArchive(t, result.Path, "Payload")
	if files["Library/Example/note.txt"] != "native map" {
		t.Fatal("native data map lost its content")
	}
}

func TestBuildRejectsInspectionDeclarations(t *testing.T) {
	spec := Spec{
		Package: Package{Identifier: "org.example.fixture", Version: "1"},
		Inspect: map[string]Inspection{"app": {Input: "vendor"}},
	}
	workspace := t.TempDir()
	_, err := Build(t.Context(), spec, nil, workspace, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "inspections require package preparation") {
		t.Fatalf("concrete build ignored inspection declarations: %v", err)
	}
	files, err := os.ReadDir(workspace)
	if err != nil || len(files) != 0 {
		t.Fatalf("concrete build produced files: %v", err)
	}
}
