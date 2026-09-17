package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/woodleighschool/stemma/internal/testproject"
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

func TestIconsAreAuthoredOnceAndPublishedAsExactBytes(t *testing.T) {
	server := httptest.NewServer(http.FileServer(http.Dir("../apple/testdata")))
	t.Cleanup(server.Close)
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	if err := testproject.Write(filename, fmt.Appendf(nil, iconProject, server.URL)); err != nil {
		t.Fatal(err)
	}
	renders := 0
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), StateDir: t.TempDir(), Icons: IconOptions{Size: 256, Renderer: func(_ context.Context, app string, size int) ([]byte, error) {
		renders++
		if _, err := os.Stat(filepath.Join(app, "Contents", "Info.plist")); err != nil {
			return nil, err
		}
		if filepath.Base(app) != "SignedFixture.app" || size != 256 {
			return nil, fmt.Errorf("rendered %s at %d pixels", app, size)
		}
		return iconPNG(t, size, uint8(renders)), nil
	}}}
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
			if resource.Icon == "exists" && len(resource.Artifacts) != 0 {
				t.Fatalf("%s: prepared %s although its asset exists", method, resource.Name)
			}
		}
		return result
	}

	// A new declaration locks and renders before its asset exists.
	statuses("prepare")
	if got := statuses("icon"); got["fixture"] != "rendered" || got["branding"] != "no application" || got["plain"] != "no icon declared" {
		t.Fatalf("first render: %v", got)
	}
	rendered, err := icon.Read(root, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses("icon"); got["fixture"] != "exists" || renders != 1 {
		t.Fatalf("existing artwork was not left alone: %v after %d renders", got, renders)
	}
	options.Icons.Force = true
	if got := statuses("icon"); got["fixture"] != "replaced" || renders != 2 {
		t.Fatalf("forced render: %v after %d renders", got, renders)
	}
	replaced, err := icon.Read(root, "fixture")
	if err != nil || bytes.Equal(replaced, rendered) {
		t.Fatalf("forced render kept the previous bytes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "stemma.lock.yaml")); err != nil {
		t.Fatal(err)
	}

	// Publication needs every declared asset, and hand-made artwork is as valid as rendered.
	published := map[string]plugin.Artifact{}
	applies := 0
	options.Handlers = map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
		if request.Method == "apply" {
			applies++
			published[request.Identity.Software] = request.Inputs["icon"]
		}
		return plugin.ReconcileResponse{}, nil
	}}
	options.Method = "apply"
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "icons/shared-artwork.png") || !strings.Contains(err.Error(), "stemma icon") || applies != 0 {
		t.Fatalf("missing asset did not stop publication: applies=%d error=%v", applies, err)
	}
	if _, err := ValidateProject(t.Context(), options); err == nil || !strings.Contains(err.Error(), "icons/shared-artwork.png") {
		t.Fatalf("validation accepted a missing asset: %v", err)
	}
	custom := iconPNG(t, 512, 200)
	if err := icon.Write(root, "shared-artwork", custom); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"fixture": replaced, "branding": custom} {
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
