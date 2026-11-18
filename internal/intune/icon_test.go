package intune

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestIconPublicationIndependentOfContent(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	req.Artifact.Filename = "example.pkg"
	desired := object{"@odata.type": pkgType, "displayName": "Example", "description": "Synthetic application", "publisher": "Example", "primaryBundleId": "org.example.app", "primaryBundleVersion": "1.0", "includedApps": []any{object{"bundleId": "org.example.app", "bundleVersion": "1.0"}}, "minimumSupportedOperatingSystem": object{"v12_0": true}}
	run := func() plugin.ReconcileResponse {
		t.Helper()
		result, err := reconcileFixture(t, c, req, desired)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	run()
	icon := func(value uint8) plugin.Artifact {
		t.Helper()
		img := image.NewRGBA(image.Rect(0, 0, 2, 2))
		img.SetRGBA(0, 0, color.RGBA{R: value, A: 255})
		var data bytes.Buffer
		if err := png.Encode(&data, img); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(t.TempDir(), "icon.png")
		if err := os.WriteFile(name, data.Bytes(), 0o400); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data.Bytes())
		return plugin.Artifact{Path: name, Filename: "icon.png", Format: "png", Size: int64(data.Len()), SHA256: hex.EncodeToString(digest[:])}
	}
	first, second := icon(10), icon(200)
	req.Inputs = map[string]plugin.Artifact{"icon": first}
	run()
	original := text(fake.app["largeIcon"].(object)["value"])
	if original == "" {
		t.Fatal("existing app did not acquire missing icon")
	}
	installerPath := req.Artifact.Path
	req.Artifact.Path = ""
	if result := run(); result.Changed() || result.Content != nil {
		t.Fatalf("unchanged icon planned writes: %+v", result)
	}
	// Changed bytes are ordinary drift: planning reports them, applying replaces them.
	unleased := second
	unleased.Path = ""
	req.Inputs["icon"] = unleased
	req.Method = "plan"
	if result := run(); !reflect.DeepEqual(result.Uploads, []plugin.Upload{{Name: plugin.UploadIcon, Filename: "icon.png", Before: first.SHA256, After: second.SHA256}}) || len(result.Fields) != 0 {
		t.Fatalf("changed icon plan: %+v", result)
	}
	if text(fake.app["largeIcon"].(object)["value"]) != original {
		t.Fatal("plan wrote icon")
	}
	req.Method = "apply"
	req.Inputs["icon"] = second
	run()
	replaced := text(fake.app["largeIcon"].(object)["value"])
	if replaced == original {
		t.Fatal("changed icon was not published")
	}
	// Without a declared icon the published artwork is left unchanged.
	req.Inputs = nil
	if result := run(); result.Changed() || text(fake.app["largeIcon"].(object)["value"]) != replaced {
		t.Fatalf("undeclared icon changed the app: %+v", result)
	}
	req.Inputs = map[string]plugin.Artifact{"icon": second}
	delete(fake.app, "largeIcon")
	run()
	if text(fake.app["largeIcon"].(object)["value"]) != replaced {
		t.Fatal("missing icon was not repaired")
	}
	if fake.creates != 1 || fake.versions != 1 || fake.commits != 1 {
		t.Fatal("icon writes recreated app or reuploaded installer")
	}

	data := []byte("updated installer")
	if err := os.WriteFile(installerPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	req.Artifact.Path = installerPath
	req.Artifact.SHA256 = hex.EncodeToString(digest[:])
	req.Artifact.Size = int64(len(data))
	result := run()
	if !slices.ContainsFunc(result.Uploads, func(upload plugin.Upload) bool {
		return upload.Name == plugin.UploadIcon && upload.After == second.SHA256
	}) {
		t.Fatalf("restored icon not reported: %+v", result)
	}
	if restored, _ := fake.app["largeIcon"].(object); text(restored["value"]) != replaced {
		t.Fatal("publication did not restore matching artwork")
	}
	if fake.creates != 1 || fake.versions != 2 || fake.commits != 2 {
		t.Fatal("installer update did not publish exactly once")
	}
	writes := fake.writes
	if result := run(); result.Changed() || fake.writes != writes {
		t.Fatalf("settled publication wrote again: %+v", result)
	}
}
