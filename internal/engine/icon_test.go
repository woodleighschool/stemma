package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

const iconProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: icons
spec:
  imports:
    - '*.software.yaml'
  destinations:
    repository:
      operation: munki
      config:
        path: repository
    graph:
      operation: intune
      config:
        token: test-token
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: example
spec:
  icon: example
  source: {path: Example.app}
  destinations:
    repository:
      pkginfo: {catalogs: [testing]}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: fixture
spec:
  icon: fixture
  source: {url: %s/fixture.pkg}
  destinations:
    repository:
      pkginfo: {catalogs: [testing]}
---
apiVersion: stemma/v1alpha1
kind: WindowsSoftware
metadata:
  name: setup
spec:
  icon: setup
  source: {path: icon.msi}
  destinations:
    graph: {type: win32}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: branding
spec:
  icon: shared-artwork
  destinations:
    repository:
      pkginfo:
        installer_type: nopkg
        version: '1'
        installcheck_script: |
          #!/bin/sh
          exit 1
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: plain
spec:
  destinations:
    repository:
      pkginfo:
        installer_type: nopkg
        version: '1'
        installcheck_script: |
          #!/bin/sh
          exit 1
`

func iconPNG(t *testing.T, edge int, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, edge, edge))
	img.SetRGBA(0, 0, color.RGBA{R: shade, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

// iconFixtures writes a Mac bundle whose ICNS wraps artwork and copies the MSI
// fixture that registers a product icon, returning both expected artworks.
func iconFixtures(t *testing.T, root string) (bundle, setup []byte) {
	t.Helper()
	bundle = iconPNG(t, 128, 33)
	var icns bytes.Buffer
	icns.WriteString("icns")
	_ = binary.Write(&icns, binary.BigEndian, uint32(16+len(bundle)))
	icns.WriteString("ic07")
	_ = binary.Write(&icns, binary.BigEndian, uint32(8+len(bundle)))
	icns.Write(bundle)
	for name, data := range map[string][]byte{
		"Example.app/Contents/Info.plist":             []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.app</string><key>CFBundleName</key><string>Example</string><key>CFBundleShortVersionString</key><string>1.2</string><key>CFBundleVersion</key><string>123</string><key>CFBundleExecutable</key><string>example</string><key>CFBundleIconFile</key><string>AppIcon</string></dict></plist>`),
		"Example.app/Contents/MacOS/example":          []byte("#!/bin/sh\nexit 0\n"),
		"Example.app/Contents/Resources/AppIcon.icns": icns.Bytes(),
	} {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "Example.app/Contents/MacOS/example"), 0o755); err != nil {
		t.Fatal(err)
	}
	msi, err := os.ReadFile("../msi/testdata/icon.msi")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "icon.msi"), msi, 0o644); err != nil {
		t.Fatal(err)
	}
	ico, err := os.ReadFile("../msi/testdata/icon.ico")
	if err != nil {
		t.Fatal(err)
	}
	// The fixture ICO holds one PNG frame after its 22-byte directory.
	return bundle, ico[22:]
}

func TestIconsAreCreatedOnceAndPublishedAsExactBytes(t *testing.T) {
	server := httptest.NewServer(http.FileServer(http.Dir("../apple/testdata")))
	t.Cleanup(server.Close)
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, fmt.Sprintf(iconProject, server.URL))
	bundleArtwork, setupArtwork := iconFixtures(t, root)
	published := map[string]plugin.Artifact{}
	applies := 0
	record := func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
		if request.Method == "apply" {
			applies++
			published[request.Identity.Software] = request.Inputs["icon"]
		}
		return plugin.ReconcileResponse{}, nil
	}
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Icons: IconOptions{Presentation: icon.Raw}, Handlers: map[string]reconcileHandler{"munki": record, "intune": record}}
	statuses := func(method string) map[string]string {
		t.Helper()
		options.Method = method
		report, err := Run(t.Context(), options)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		result := map[string]string{}
		for _, resource := range report.Resources {
			result[resource.Name] = resource.Icon
			if resource.Icon == "unchanged" && len(resource.Artifacts) != 0 {
				t.Fatalf("%s: prepared %s although its asset exists", method, resource.Name)
			}
		}
		return result
	}
	assetIs := func(name string, want []byte) {
		t.Helper()
		got, err := icon.Read(root, name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("icons/%s.png holds %d bytes, want %d: %v", name, len(got), len(want), err)
		}
	}

	// A new declaration locks and extracts before its asset exists; the raw
	// presentation writes Mac and Windows artwork alike, and a bundle without
	// a PNG-backed icon file has nothing portable to write.
	statuses("prepare")
	want := map[string]string{"example": "created raw", "setup": "created raw", "fixture": "no artwork", "branding": "no artwork", "plain": "no icon declared"}
	if got := statuses("icon"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("first run: %v", got)
	}
	assetIs("example", bundleArtwork)
	assetIs("setup", setupArtwork)
	if got := statuses("icon"); got["example"] != "unchanged" || got["setup"] != "unchanged" {
		t.Fatalf("existing artwork changed: %v", got)
	}
	custom := iconPNG(t, 512, 200)
	if err := icon.Write(root, "example", custom); err != nil {
		t.Fatal(err)
	}
	if got := statuses("icon"); got["example"] != "unchanged" {
		t.Fatalf("hand-made artwork changed: %v", got)
	}
	assetIs("example", custom)
	options.Icons.Force = true
	if got := statuses("icon"); got["example"] != "created raw" || got["setup"] != "created raw" {
		t.Fatalf("forced run: %v", got)
	}
	assetIs("example", bundleArtwork)
	if _, err := os.Stat(filepath.Join(root, "stemma.lock.yaml")); err != nil {
		t.Fatal(err)
	}

	// Publication needs every declared asset, and hand-made artwork is as valid as extracted.
	if err := icon.Write(root, "fixture", custom); err != nil {
		t.Fatal(err)
	}
	options.Method = "apply"
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "icons/shared-artwork.png") || !strings.Contains(err.Error(), "stemma icon") || applies != 0 {
		t.Fatalf("missing asset did not stop publication: applies=%d error=%v", applies, err)
	}
	if _, err := ValidateProject(t.Context(), options); err == nil || !strings.Contains(err.Error(), "icons/shared-artwork.png") {
		t.Fatalf("validation accepted a missing asset: %v", err)
	}
	if err := icon.Write(root, "shared-artwork", custom); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"example": bundleArtwork, "setup": setupArtwork, "fixture": custom, "branding": custom} {
		digest := sha256.Sum256(want)
		artifact := published[name]
		if artifact.SHA256 != hex.EncodeToString(digest[:]) || artifact.Format != "png" || artifact.Size != int64(len(want)) {
			t.Fatalf("%s published %+v", name, artifact)
		}
	}
	if artifact, declared := published["plain"]; !declared || artifact.Path != "" {
		t.Fatalf("undeclared icon reached the destination: %+v", artifact)
	}
}
