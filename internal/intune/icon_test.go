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
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestIconPublicationIndependentOfContent(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.expectedAPI = "beta"
	req := fixtureRequest(t)
	req.Artifact.Filename = "example.pkg"
	req.Metadata = raw(object{"@odata.type": pkgType, "displayName": "Example", "description": "Synthetic application", "publisher": "Example", "primaryBundleId": "org.example.app", "primaryBundleVersion": "1.0", "includedApps": []any{object{"bundleId": "org.example.app", "bundleVersion": "1.0"}}, "minimumSupportedOperatingSystem": object{"v12_0": true}})
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	run := func() plugin.ReconcileResponse {
		t.Helper()
		result, err := c.handle(t.Context(), req, desired)
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
	if result := run(); len(result.Changes) != 0 {
		t.Fatalf("unchanged icon planned writes: %+v", result.Changes)
	}
	// Changed bytes are ordinary drift: planning reports them, applying replaces them.
	req.Inputs["icon"] = second
	req.Method = "plan"
	if result := run(); len(result.Changes) != 1 || result.Changes[0].Field != "largeIcon" {
		t.Fatalf("changed icon plan: %+v", result.Changes)
	}
	if text(fake.app["largeIcon"].(object)["value"]) != original {
		t.Fatal("plan wrote icon")
	}
	req.Method = "apply"
	run()
	replaced := text(fake.app["largeIcon"].(object)["value"])
	if replaced == original {
		t.Fatal("changed icon was not published")
	}
	// Without a declared icon the published artwork is left unchanged.
	req.Inputs = nil
	if result := run(); len(result.Changes) != 0 || text(fake.app["largeIcon"].(object)["value"]) != replaced {
		t.Fatalf("undeclared icon changed the app: %+v", result.Changes)
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
}
