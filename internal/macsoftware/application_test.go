package macsoftware

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/testdiskimage"
	"github.com/woodleighschool/stemma/plugin"
)

func TestBundleMaterializesTheSelectedApplication(t *testing.T) {
	root := applicationFixture(t)
	image := filepath.Join(t.TempDir(), "Example.dmg")
	if err := testdiskimage.Write(image, root); err != nil {
		t.Fatal(err)
	}
	for _, input := range []plugin.Artifact{
		{Path: image, Filename: "Example.dmg", Format: "dmg"},
		{Path: filepath.Join(root, "Example.app"), Filename: "Example.app", Tree: true},
	} {
		t.Run(input.Filename, func(t *testing.T) {
			outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			// The workspace is the engine's, which creates nothing under it beforehand.
			bundle, err := Bundle(t.Context(), outputs["installer"], filepath.Join(t.TempDir(), "icon"))
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(bundle) != "Example.app" {
				t.Fatalf("bundle path %q", bundle)
			}
			for _, name := range []string{"Contents/Info.plist", "Contents/MacOS/example", "Contents/Resources/icon.png"} {
				if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if _, err := Bundle(t.Context(), plugin.Artifact{Path: image, Format: "dmg"}, t.TempDir()); !errors.Is(err, ErrNoApplication) {
		t.Fatalf("installer without a selected application: %v", err)
	}
}
