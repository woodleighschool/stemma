package macsoftware

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testdiskimage"
	"github.com/woodleighschool/stemma/plugin"
)

func applicationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{
		"Example.app/Contents/Info.plist":           []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.app</string><key>CFBundleName</key><string>Example</string><key>CFBundleShortVersionString</key><string>1.2</string><key>CFBundleVersion</key><string>123</string><key>CFBundleExecutable</key><string>example</string><key>CFBundleIconFile</key><string>AppIcon</string></dict></plist>`),
		"Example.app/Contents/MacOS/example":        []byte("#!/bin/sh\nexit 97\n"),
		"Example.app/Contents/Resources/manual.pdf": []byte("not an icon"),
	}
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
	outputs, err := Prepare(t.Context(), spec, Request{Input: input, Workspace: t.TempDir()})
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
	if installer.Format != "pkg" || installer.Version != "123" || app.App.BundleID != "org.example.app" || app.InstalledPath != "/Applications/Example.app" || len(outputs) != 1 {
		t.Fatalf("outputs=%+v app=%+v", outputs, app)
	}
	if _, err := apple.VerifyPackage(t.Context(), installer.Path, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("built package claimed a signer: %v", err)
	}
	again, err := Prepare(t.Context(), spec, Request{Input: input, Workspace: t.TempDir()})
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
	outputs, err := Prepare(t.Context(), Spec{Application: &Application{InstalledPath: "/Applications/Renamed.app"}}, Request{Input: plugin.Artifact{Path: filename, Filename: "Example.dmg", Format: "dmg"}, Workspace: t.TempDir()})
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

func TestDMGMetadataDoesNotExtract(t *testing.T) {
	source := applicationFixture(t)
	filename := filepath.Join(t.TempDir(), "Example.dmg")
	if err := testdiskimage.Write(filename, source); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	selected, err := selectPayload(t.Context(), Spec{}, plugin.Artifact{Path: filename, Filename: "Example.dmg"}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.close()
	facts, err := selected.inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Subjects) != 1 || facts.Subjects[0].App.BundleID != "org.example.app" {
		t.Fatalf("metadata: %+v", facts)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 0 {
		t.Fatalf("metadata inspection materialized payload: %v, %v", entries, err)
	}
}

func TestDMGPackageIsPublishedAsALocalFile(t *testing.T) {
	data, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Example.pkg"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "Example.dmg")
	if err := testdiskimage.Write(filename, source); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	outputs, err := Prepare(t.Context(), Spec{}, Request{Input: plugin.Artifact{Path: filename, Filename: "Example.dmg", Format: "dmg"}, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	installer := outputs["installer"]
	hash := sha256.Sum256(data)
	if installer.Format != "pkg" || installer.Filename != "Example.pkg" || installer.SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("installer = %+v", installer)
	}
	if published, err := os.ReadFile(installer.Path); err != nil || !bytes.Equal(published, data) {
		t.Fatalf("published package differs from the image's: %v", err)
	}
}

func TestDMGSignatureVerifiesSelectedApplication(t *testing.T) {
	const signer = "apple:developer-id:SMLKBTR495"
	for _, modified := range []bool{false, true} {
		name := "valid"
		if modified {
			name = "modified executable"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			app := filepath.Join(root, "SignedFixture.app")
			if err := os.CopyFS(app, os.DirFS("../apple/testdata/SignedFixture.app")); err != nil {
				t.Fatal(err)
			}
			if modified {
				filename := filepath.Join(app, "Contents/MacOS/fixture")
				data, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				// Modify signed code inside the first Mach-O architecture.
				data[16384+4096] ^= 0x40
				if err := os.WriteFile(filename, data, 0755); err != nil {
					t.Fatal(err)
				}
			}
			filename := filepath.Join(t.TempDir(), "Example.dmg")
			if err := testdiskimage.Write(filename, root); err != nil {
				t.Fatal(err)
			}
			input := plugin.Artifact{Path: filename, Filename: "Example.dmg", Format: "dmg"}
			workspace := t.TempDir()
			outputs, err := Prepare(t.Context(), Spec{Signature: &signature.Policy{Signer: signer}}, Request{Input: input, Workspace: workspace})
			// The application is verified inside the image; only the retained
			// installer reaches the workspace.
			if entries, readErr := os.ReadDir(workspace); readErr != nil || len(entries) > 1 || len(entries) == 1 && entries[0].Name() != "Example.dmg" {
				t.Fatalf("verification wrote to the workspace: %v, %v", entries, readErr)
			}
			if modified {
				if err == nil {
					t.Fatal("verification ignored modified executable")
				}
				if _, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir(), DeriveSignature: true}); err == nil {
					t.Fatal("derivation ignored modified executable")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var evidence signature.Result
			if err := json.Unmarshal(outputs["installer"].Evidence["signature"], &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Signer != signer || evidence.Name != "Woodleigh School" || evidence.Target != "SignedFixture.app" {
				t.Fatalf("signature evidence: %+v", evidence)
			}
			if local, err := apple.VerifyApp(t.Context(), app, signature.Signer{}); err != nil || local != evidence {
				t.Fatalf("evidence from the image %+v differs from the bundle on disk %+v: %v", evidence, local, err)
			}
			if _, err := Prepare(t.Context(), Spec{Signature: &signature.Policy{Signer: "apple:developer-id:AAAAAAAAAA"}}, Request{Input: input, Workspace: t.TempDir()}); !errors.Is(err, signature.ErrMismatch) {
				t.Fatalf("unexpected signer accepted: %v", err)
			}
			derived, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir(), DeriveSignature: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(derived["installer"].Evidence["signature"], &evidence); err != nil || evidence.Signer != signer {
				t.Fatalf("derived evidence: %+v: %v", evidence, err)
			}
		})
	}
}
