package macsoftware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
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

func TestZIPApplicationIsPublishedInADiskImage(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "Example.zip")
	testarchive.Zip(t, filename, applicationFixture(t))
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
	if installer.Format != "dmg" || installer.Filename != "Example.dmg" || installer.Version != "123" || app.Path != "Example.app" || app.InstalledPath != "/Applications/Example.app" || len(outputs) != 1 {
		t.Fatalf("outputs=%+v app=%+v", outputs, app)
	}
	reopened, err := inspect.Read(t.Context(), installer.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Subjects) != 2 || reopened.Subjects[0].SHA256 != installer.SHA256 || reopened.Subjects[1].Path != app.Path || !reflect.DeepEqual(reopened.Subjects[1].App, app.App) {
		t.Fatalf("image holds %+v, evidence describes %+v", reopened.Subjects, app)
	}
	again, err := Prepare(t.Context(), spec, Request{Input: input, Workspace: t.TempDir()})
	if err != nil || again["installer"].SHA256 != installer.SHA256 {
		t.Fatalf("nondeterministic image: %v", err)
	}
}

// TestZIPSignatureVerifiesTheApplication prepares the shape of a GitHub release:
// a versioned ZIP holding one Developer ID signed bundle.
func TestZIPSignatureVerifiesTheApplication(t *testing.T) {
	const signer = "apple:developer-id:SMLKBTR495"
	release := t.TempDir()
	if err := os.CopyFS(filepath.Join(release, "WoodSweep.app"), os.DirFS("../apple/testdata/SignedFixture.app")); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "WoodSweep-1.2.3.zip")
	testarchive.Zip(t, filename, release)
	input := plugin.Artifact{Path: filename, Filename: "WoodSweep-1.2.3.zip", Format: "zip"}
	outputs, err := Prepare(t.Context(), Spec{Signature: &signature.Policy{Signer: signer}}, Request{Input: input, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	installer := outputs["installer"]
	var evidence signature.Result
	if err := json.Unmarshal(installer.Evidence["signature"], &evidence); err != nil {
		t.Fatal(err)
	}
	if installer.Format != "dmg" || evidence.Signer != signer || evidence.Target != "WoodSweep.app" {
		t.Fatalf("installer=%+v evidence=%+v", installer, evidence)
	}
	image, err := diskimage.Open(t.Context(), installer.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	if inside, err := apple.VerifyAppFS(t.Context(), image, "WoodSweep.app", signature.Signer{}); err != nil || inside != evidence {
		t.Fatalf("bundle in the image verifies as %+v, evidence is %+v: %v", inside, evidence, err)
	}
	workspace := t.TempDir()
	if _, err := Prepare(t.Context(), Spec{Signature: &signature.Policy{Signer: "apple:developer-id:AAAAAAAAAA"}}, Request{Input: input, Workspace: workspace}); !errors.Is(err, signature.ErrMismatch) {
		t.Fatalf("unexpected signer accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "WoodSweep.dmg")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("image built for a rejected application: %v", err)
	}
}

func TestDMGApplicationRetainsVendorBytes(t *testing.T) {
	root := applicationFixture(t)
	filename := filepath.Join(t.TempDir(), "Example.dmg")
	testdiskimage.Write(t, filename, root)
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

func TestPrepareRejectsUnrecognizedContainerFormat(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "download.bin")
	testdiskimage.Write(t, filename, applicationFixture(t))
	_, err := Prepare(t.Context(), Spec{}, Request{Input: plugin.Artifact{Path: filename, Filename: "download.bin"}, Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("disk image was accepted as a scalar PKG")
	}
}

func TestDMGFactsListEveryApplication(t *testing.T) {
	fixture := filepath.Join(applicationFixture(t), "Example.app")
	source := t.TempDir()
	for _, name := range []string{"Suite/Example.app", "Suite/Other.app"} {
		if err := os.CopyFS(filepath.Join(source, filepath.FromSlash(name)), os.DirFS(fixture)); err != nil {
			t.Fatal(err)
		}
	}
	filename := filepath.Join(t.TempDir(), "Suite.dmg")
	testdiskimage.Write(t, filename, source)
	input := plugin.Artifact{Path: filename, Filename: "Suite.dmg", Format: "dmg"}
	if _, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "Suite/Example.app, Suite/Other.app") {
		t.Fatalf("ambiguous selection: %v", err)
	}
	workspace := t.TempDir()
	outputs, err := Prepare(t.Context(), Spec{Application: &Application{Path: "Suite/Example.app"}}, Request{Input: input, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	subjects := outputs["installer"].Facts.Subjects
	if len(subjects) != 3 || subjects[0].SHA256 != outputs["installer"].SHA256 {
		t.Fatalf("facts = %+v", subjects)
	}
	if subjects[1].ID != "Suite/Example.app" || subjects[1].InstalledPath != "/Applications/Example.app" || subjects[2].ID != "Suite/Other.app" || subjects[2].InstalledPath != "" || subjects[2].App == nil {
		t.Fatalf("applications = %+v", subjects[1:])
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 1 || entries[0].Name() != "Suite.dmg" {
		t.Fatalf("inventory wrote to the workspace: %v, %v", entries, err)
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
	testdiskimage.Write(t, filename, source)
	input := plugin.Artifact{Path: filename, Filename: "Example.dmg", Format: "dmg"}
	outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	installer := outputs["installer"]
	hash := sha256.Sum256(data)
	if installer.Format != "pkg" || installer.Filename != "Example.pkg" || installer.SHA256 != hex.EncodeToString(hash[:]) || installer.Facts.Subjects[0].SHA256 != installer.SHA256 {
		t.Fatalf("installer = %+v", installer)
	}
	if published, err := os.ReadFile(installer.Path); err != nil || !bytes.Equal(published, data) {
		t.Fatalf("published package differs from the image's: %v", err)
	}
	// The image holds no application, so the selector names one in the package.
	outputs, err = Prepare(t.Context(), Spec{Application: &Application{Path: "Payload/SignedFixture.app"}}, Request{Input: input, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var app plugin.Subject
	if err := json.Unmarshal(outputs["installer"].Evidence["macos.application"], &app); err != nil || outputs["installer"].Format != "pkg" || app.InstalledPath != "/Applications/SignedFixture.app" {
		t.Fatalf("application in package: %+v, %v", app, err)
	}
}

func TestDMGSignatureVerifiesInsideTheImage(t *testing.T) {
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
			testdiskimage.Write(t, filename, root)
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

func TestDMGSignatureVerifiesEveryApplication(t *testing.T) {
	const signer = "apple:developer-id:SMLKBTR495"
	root := t.TempDir()
	for _, name := range []string{"Suite/Main.app", "Suite/Companion.app"} {
		if err := os.CopyFS(filepath.Join(root, filepath.FromSlash(name)), os.DirFS("../apple/testdata/SignedFixture.app")); err != nil {
			t.Fatal(err)
		}
	}
	signed := filepath.Join(t.TempDir(), "Signed.dmg")
	testdiskimage.Write(t, signed, root)
	spec := Spec{Application: &Application{Path: "Suite/Main.app"}, Signature: &signature.Policy{Signer: signer}}
	outputs, err := Prepare(t.Context(), spec, Request{Input: plugin.Artifact{Path: signed, Filename: "Signed.dmg", Format: "dmg"}, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var evidence signature.Result
	if err := json.Unmarshal(outputs["installer"].Evidence["signature"], &evidence); err != nil || evidence.Signer != signer || evidence.Target != "Companion.app, Main.app" {
		t.Fatalf("signature evidence: %+v, %v", evidence, err)
	}
	unsigned := filepath.Join(applicationFixture(t), "Example.app")
	if err := os.CopyFS(filepath.Join(root, "Suite/Example.app"), os.DirFS(unsigned)); err != nil {
		t.Fatal(err)
	}
	mixed := filepath.Join(t.TempDir(), "Mixed.dmg")
	testdiskimage.Write(t, mixed, root)
	input := plugin.Artifact{Path: mixed, Filename: "Mixed.dmg", Format: "dmg"}
	for name, request := range map[string]Request{
		"declared": {Input: input, Workspace: t.TempDir()},
		"derived":  {Input: input, Workspace: t.TempDir(), DeriveSignature: true},
	} {
		t.Run(name, func(t *testing.T) {
			spec := spec
			if request.DeriveSignature {
				spec.Signature = nil
			}
			if _, err := Prepare(t.Context(), spec, request); err == nil || !strings.Contains(err.Error(), "Suite/Example.app") {
				t.Fatalf("unsigned companion accepted: %v", err)
			}
		})
	}
}
