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
		result, err := c.handle(t.Context(), req, configuration{}, desired)
		if err != nil {
			t.Fatal(err)
		}
		req.Binding = result.Binding
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
	portable, native := icon(10), icon(200)
	req.Inputs = map[string]plugin.Artifact{"icon": portable}
	run()
	original := text(fake.app["largeIcon"].(object)["value"])
	if original == "" {
		t.Fatal("existing app did not acquire missing icon")
	}
	req.Inputs["icon"] = native
	if result := run(); len(result.Changes) != 0 {
		t.Fatal("normal apply changed existing icon")
	}
	req.RefreshIcons = true
	req.Method = "plan"
	if result := run(); len(result.Changes) != 1 || result.Changes[0].Field != "largeIcon" {
		t.Fatalf("refresh plan: %+v", result.Changes)
	}
	if text(fake.app["largeIcon"].(object)["value"]) != original {
		t.Fatal("plan wrote icon")
	}
	req.Method = "apply"
	run()
	improved := text(fake.app["largeIcon"].(object)["value"])
	if improved == original {
		t.Fatal("refresh retained portable icon")
	}
	req.RefreshIcons = false
	req.Inputs["icon"] = portable
	run()
	if text(fake.app["largeIcon"].(object)["value"]) != improved {
		t.Fatal("portable runner downgraded icon")
	}
	delete(fake.app, "largeIcon")
	run()
	if text(fake.app["largeIcon"].(object)["value"]) != original {
		t.Fatal("missing icon was not repaired")
	}
	if fake.creates != 1 || fake.versions != 1 || fake.commits != 1 {
		t.Fatal("icon writes recreated app or reuploaded installer")
	}
}
