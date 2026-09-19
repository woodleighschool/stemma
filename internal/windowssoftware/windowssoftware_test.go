package windowssoftware

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/plugin"
)

func writeFixture(t *testing.T, root, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestSetupTreePreservesFilesAndSelectsMSIEvidence(t *testing.T) {
	msi, err := os.ReadFile("../msi/testdata/test.msi")
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeFixture(t, source, "bin/vendor.msi", msi, 0o640)
	companion := writeFixture(t, t.TempDir(), "settings.ini", []byte("[Install]\nSilent=1\n"), 0o604)
	inputs := map[string]plugin.Artifact{"source": {Path: source, Tree: true, EntryPoint: "bin/vendor.msi", Version: "stale", Evidence: map[string]json.RawMessage{"vendor.probe": json.RawMessage(`{"channel":"stable"}`)}}, "file:settings.ini": {Path: companion}}
	spec := Spec{Content: &Content{Files: map[string]plugin.Input{"settings.ini": {}}}}
	outputs, err := Prepare(t.Context(), spec, inputs, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	artifact := outputs["installer"]
	if !artifact.Tree || artifact.EntryPoint != "bin/vendor.msi" || artifact.Version != "1.2.3" {
		t.Fatalf("prepared installer: %#v", artifact)
	}
	var selected plugin.Subject
	if err := json.Unmarshal(artifact.Evidence["windows.installer"], &selected); err != nil {
		t.Fatal(err)
	}
	if selected.MSI == nil || selected.Path != "bin/vendor.msi" || selected.MSI.ProductCode != "{8B2D32B7-0BE9-4CF9-B1E7-42C27753A6B8}" || string(artifact.Evidence["vendor.probe"]) != `{"channel":"stable"}` {
		t.Fatal("incorrect selected MSI evidence")
	}
	for name, mode := range map[string]os.FileMode{"bin/vendor.msi": 0o640, "settings.ini": 0o604} {
		info, err := os.Stat(filepath.Join(artifact.Path, filepath.FromSlash(name)))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode %s: %v %v", name, info, err)
		}
	}
	copied, err := os.ReadFile(filepath.Join(artifact.Path, "bin/vendor.msi"))
	if err != nil || !bytes.Equal(copied, msi) {
		t.Fatal("vendor MSI bytes changed")
	}
	repeated, err := Prepare(t.Context(), spec, inputs, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := archive.Pack(t.Context(), artifact.Path, &first); err != nil {
		t.Fatal(err)
	}
	if err := archive.Pack(t.Context(), repeated["installer"].Path, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("identical input produced different snapshots")
	}
}

func TestCompanionScriptReplacesSelectedMSIEvidence(t *testing.T) {
	msi, err := os.ReadFile("../msi/testdata/test.msi")
	if err != nil {
		t.Fatal(err)
	}
	vendor := writeFixture(t, t.TempDir(), "vendor.msi", msi, 0o644)
	marker := filepath.Join(t.TempDir(), "executed")
	script := []byte("#!/bin/sh\ntouch " + marker + "\nexit 91\n")
	command := writeFixture(t, t.TempDir(), "install.cmd", script, 0o755)
	inputs := map[string]plugin.Artifact{"source": {Path: vendor, Filename: "vendor.msi", Version: "1.2.3", Evidence: map[string]json.RawMessage{"windows.installer": json.RawMessage(`{"msi":{"productCode":"stale"}}`), "vendor.probe": json.RawMessage(`true`)}}, "file:install.cmd": {Path: command}}
	spec := Spec{Content: &Content{SetupFile: "install.cmd", Files: map[string]plugin.Input{"install.cmd": {}}}}
	outputs, err := Prepare(t.Context(), spec, inputs, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	artifact := outputs["installer"]
	if _, exists := artifact.Evidence["windows.installer"]; exists {
		t.Fatal("wrapper inherited stale selected MSI evidence")
	}
	if artifact.EntryPoint != "install.cmd" || artifact.Version != "" || string(artifact.Evidence["vendor.probe"]) != "true" {
		t.Fatal("wrapper evidence lost ownership boundary")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("installer script executed: %v", err)
	}
	copied, err := os.ReadFile(filepath.Join(artifact.Path, "install.cmd"))
	if err != nil || !bytes.Equal(script, copied) {
		t.Fatal("script changed")
	}
}

func TestCompanionPathsFailBeforePublishing(t *testing.T) {
	for _, test := range []struct {
		name                  string
		sourceTree            bool
		target, second, setup string
	}{
		{name: "escape", target: "../escaped"},
		{name: "Windows alias", target: "SETUP.EXE"},
		{name: "device path", target: "NUL.txt"},
		{name: "case-conflicting parent", sourceTree: true, target: "data/new.ini"},
		{name: "overlapping companions", target: "assets", second: "assets/settings.ini"},
		{name: "setup escape", target: "settings.ini", setup: "../setup.exe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			file := writeFixture(t, source, "setup.exe", []byte("unexecuted installer"), 0o644)
			input := plugin.Artifact{Path: file, Filename: "setup.exe"}
			if test.sourceTree {
				writeFixture(t, source, "Data/vendor.ini", []byte("vendor"), 0o644)
				input = plugin.Artifact{Path: source, Tree: true, EntryPoint: "setup.exe"}
			}
			companion := writeFixture(t, t.TempDir(), "companion", []byte("settings"), 0o644)
			inputs := map[string]plugin.Artifact{"source": input, "file:" + test.target: {Path: companion}}
			content := &Content{SetupFile: test.setup, Files: map[string]plugin.Input{test.target: {}}}
			if test.second != "" {
				content.Files[test.second] = plugin.Input{}
				inputs["file:"+test.second] = plugin.Artifact{Path: companion}
			}
			workspace := t.TempDir()
			if _, err := Prepare(t.Context(), Spec{Content: content}, inputs, workspace, false); err == nil {
				t.Fatal("unsafe content accepted")
			}
			entries, err := os.ReadDir(workspace)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed preparation left outputs: %v %v", entries, err)
			}
		})
	}
}

func TestCompanionSymlinkIsRejected(t *testing.T) {
	vendor := writeFixture(t, t.TempDir(), "setup.exe", []byte("unexecuted vendor"), 0o644)
	input := writeFixture(t, t.TempDir(), "settings.ini", []byte("settings"), 0o644)
	link := filepath.Join(t.TempDir(), "link.ini")
	if err := os.Symlink(input, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spec := Spec{Content: &Content{Files: map[string]plugin.Input{"settings.ini": {}}}}
	inputs := map[string]plugin.Artifact{"source": {Path: vendor, Filename: "setup.exe"}, "file:settings.ini": {Path: link}}
	workspace := t.TempDir()
	if _, err := Prepare(t.Context(), spec, inputs, workspace, false); err == nil {
		t.Fatal("symlinked companion accepted")
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed copy left output: %v %v", entries, err)
	}
}
