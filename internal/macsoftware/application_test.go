package macsoftware

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/testdiskimage"
	"github.com/woodleighschool/stemma/plugin"
)

// bundleInputs offers one application as a disk image and as a tree, which
// preparation wraps in a package.
func bundleInputs(t *testing.T, root string) []plugin.Artifact {
	t.Helper()
	image := filepath.Join(t.TempDir(), "Example.dmg")
	if err := testdiskimage.Write(image, root); err != nil {
		t.Fatal(err)
	}
	return []plugin.Artifact{
		{Path: image, Filename: "Example.dmg", Format: "dmg"},
		{Path: filepath.Join(root, "Example.app"), Filename: "Example.app", Tree: true},
	}
}

func bundleFiles(t *testing.T, bundle string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(bundle, func(current string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			relative, _ := filepath.Rel(bundle, current)
			files = append(files, filepath.ToSlash(relative))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestBundleMaterializesWhatAnIconRenderReads(t *testing.T) {
	root := applicationFixture(t)
	resources := filepath.Join(root, "Example.app/Contents/Resources")
	if err := os.WriteFile(filepath.Join(resources, "Source.icns"), []byte("icns"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("Source.icns", filepath.Join(resources, "AppIcon.icns")); err != nil {
		t.Skip(err)
	}
	inputs := bundleInputs(t, root)
	for _, input := range inputs {
		t.Run(input.Filename, func(t *testing.T) {
			outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			// The workspace is the engine's, which creates nothing under it beforehand.
			subject, err := Icon(t.Context(), outputs["installer"], filepath.Join(t.TempDir(), "icon"), icon.Glassy)
			bundle := subject.Path
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(bundle) != "Example.app" {
				t.Fatalf("bundle path %q", bundle)
			}
			want := []string{"Contents/Info.plist", "Contents/MacOS/example", "Contents/Resources/AppIcon.icns", "Contents/Resources/Source.icns"}
			if files := bundleFiles(t, bundle); !slices.Equal(files, want) {
				t.Fatalf("bundle holds %v, want %v", files, want)
			}
		})
	}
	if _, err := Icon(t.Context(), plugin.Artifact{Path: inputs[0].Path, Format: "dmg"}, t.TempDir(), icon.Glassy); !errors.Is(err, ErrNoApplication) {
		t.Fatalf("installer without a selected application: %v", err)
	}
}

func TestBundleWithALinkedExecutableIsMaterializedWhole(t *testing.T) {
	root := applicationFixture(t)
	contents := filepath.Join(root, "Example.app/Contents")
	if err := os.MkdirAll(filepath.Join(contents, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(contents, "MacOS/example"), filepath.Join(contents, "Frameworks/example")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Frameworks/example", filepath.Join(contents, "MacOS/example")); err != nil {
		t.Fatal(err)
	}
	for _, input := range bundleInputs(t, root) {
		t.Run(input.Filename, func(t *testing.T) {
			outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			subject, err := Icon(t.Context(), outputs["installer"], filepath.Join(t.TempDir(), "icon"), icon.Glassy)
			bundle := subject.Path
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"Contents/Frameworks/example", "Contents/Info.plist", "Contents/MacOS/example", "Contents/Resources/manual.pdf"}
			if files := bundleFiles(t, bundle); !slices.Equal(files, want) {
				t.Fatalf("bundle holds %v, want %v", files, want)
			}
		})
	}
}

func TestBundleWithoutItsExecutableFails(t *testing.T) {
	root := applicationFixture(t)
	if err := os.Remove(filepath.Join(root, "Example.app/Contents/MacOS/example")); err != nil {
		t.Fatal(err)
	}
	for _, input := range bundleInputs(t, root) {
		t.Run(input.Filename, func(t *testing.T) {
			outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if subject, err := Icon(t.Context(), outputs["installer"], filepath.Join(t.TempDir(), "icon"), icon.Glassy); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("bundle %q without an executable: %v", subject.Path, err)
			}
		})
	}
}

func TestIconExtractsOnlyWhatItsPresentationNeeds(t *testing.T) {
	root := applicationFixture(t)
	input := plugin.Artifact{Path: filepath.Join(root, "Example.app"), Filename: "Example.app", Tree: true}
	outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The fixture declares an icon file it does not carry.
	subject, err := Icon(t.Context(), outputs["installer"], filepath.Join(t.TempDir(), "icon"), icon.Glassy)
	if err != nil || filepath.Base(subject.Path) != "Example.app" || subject.Artwork != nil {
		t.Fatalf("subject without artwork: %+v %v", subject, err)
	}
	if _, err := Icon(t.Context(), outputs["installer"], filepath.Join(t.TempDir(), "icon"), icon.Raw); !errors.Is(err, icon.ErrNoArtwork) {
		t.Fatalf("raw icon without artwork: %v", err)
	}
	var artwork bytes.Buffer
	if err := png.Encode(&artwork, image.NewNRGBA(image.Rect(0, 0, 128, 128))); err != nil {
		t.Fatal(err)
	}
	var icns bytes.Buffer
	icns.WriteString("icns")
	_ = binary.Write(&icns, binary.BigEndian, uint32(16+artwork.Len()))
	icns.WriteString("ic07")
	_ = binary.Write(&icns, binary.BigEndian, uint32(8+artwork.Len()))
	icns.Write(artwork.Bytes())
	if err := os.WriteFile(filepath.Join(root, "Example.app/Contents/Resources/AppIcon.icns"), icns.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "Example.app/Contents/MacOS/example")); err != nil {
		t.Fatal(err)
	}
	if outputs, err = Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "icon")
	subject, err = Icon(t.Context(), outputs["installer"], workspace, icon.Raw)
	if err != nil || !bytes.Equal(subject.Artwork, artwork.Bytes()) || subject.Path != "" {
		t.Fatalf("subject with ICNS artwork: %d bytes %v", len(subject.Artwork), err)
	}
	want := []string{"Contents/Info.plist", "Contents/Resources/AppIcon.icns"}
	if files := bundleFiles(t, filepath.Join(workspace, "expanded/Example.app")); !slices.Equal(files, want) {
		t.Fatalf("raw artwork extracted %v, want %v", files, want)
	}
}
