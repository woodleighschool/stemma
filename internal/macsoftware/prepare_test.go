package macsoftware

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/testdiskimage"
	"github.com/woodleighschool/stemma/plugin"
)

func applicationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{
		"Example.app/Contents/Info.plist":    []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.app</string><key>CFBundleName</key><string>Example</string><key>CFBundleShortVersionString</key><string>1.2</string><key>CFBundleVersion</key><string>123</string><key>CFBundleExecutable</key><string>example</string><key>CFBundleIconFile</key><string>icon.png</string></dict></plist>`),
		"Example.app/Contents/MacOS/example": []byte("#!/bin/sh\nexit 97\n"),
	}
	var icon bytes.Buffer
	if err := png.Encode(&icon, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	files["Example.app/Contents/Resources/icon.png"] = icon.Bytes()
	for name, data := range files {
		destination := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "Example.app/Contents/MacOS/example"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestZIPApplicationProducesPackageAndSelectedEvidence(t *testing.T) {
	root := applicationFixture(t)
	filename := filepath.Join(t.TempDir(), "Example.zip")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name, _ = filepath.Rel(root, name)
		header.Name = filepath.ToSlash(header.Name)
		out, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	input := plugin.Artifact{Path: filename, Filename: "Example.zip", Format: "zip", Evidence: map[string]json.RawMessage{"vendor.probe": json.RawMessage(`{"release":"preview"}`)}}
	spec := Spec{Application: &Application{Path: "Example.app", VersionKey: "CFBundleVersion"}}
	outputs, err := Prepare(t.Context(), spec, input, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	installer := outputs["installer"]
	if string(installer.Evidence["vendor.probe"]) != string(input.Evidence["vendor.probe"]) {
		t.Fatal("upstream evidence lost")
	}
	var app plugin.Subject
	if err := json.Unmarshal(installer.Evidence["macos.application"], &app); err != nil {
		t.Fatal(err)
	}
	if installer.Format != "pkg" || installer.Version != "123" || app.App.BundleID != "org.example.app" || app.InstalledPath != "/Applications/Example.app" || outputs["icon"].Format != "png" {
		t.Fatalf("outputs=%+v app=%+v", outputs, app)
	}
	if _, err := apple.VerifyPackage(installer.Path, apple.Policy{RequireIntegrity: true}); err != nil {
		t.Fatal(err)
	}
	again, err := Prepare(t.Context(), spec, input, t.TempDir(), time.Time{})
	if err != nil || again["installer"].SHA256 != installer.SHA256 {
		t.Fatalf("nondeterministic package: %v", err)
	}
}

func TestDMGApplicationRetainsVendorBytes(t *testing.T) {
	root := applicationFixture(t)
	filename := filepath.Join(t.TempDir(), "Example.dmg")
	if err := testdiskimage.Write(filename, root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	outputs, err := Prepare(t.Context(), Spec{Application: &Application{InstalledPath: "/Applications/Renamed.app"}}, plugin.Artifact{Path: filename, Filename: "Example.dmg", Format: "dmg"}, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	installer := outputs["installer"]
	var app plugin.Subject
	if err := json.Unmarshal(installer.Evidence["macos.application"], &app); err != nil {
		t.Fatal(err)
	}
	if installer.Format != "dmg" || installer.SHA256 != hex.EncodeToString(hash[:]) || app.Path != "Example.app" || app.InstalledPath != "/Applications/Renamed.app" {
		t.Fatalf("installer=%+v app=%+v", installer, app)
	}
}
