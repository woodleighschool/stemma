package macsoftware

import (
	"bytes"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/plugin"
)

// The signatures come from a native codesign fixture, independent of our writer.
func metadataApplication(t *testing.T) plugin.Artifact {
	t.Helper()
	const app = "GenericFixture.app"
	source, err := diskimage.Open(t.Context(), "../apple/testdata/generic.dmg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	stage := filepath.Join(t.TempDir(), "stage")
	if _, err := source.Extract(t.Context(), stage, app, nil); err != nil {
		t.Fatal(err)
	}
	attributes := map[string]map[string][]byte{}
	if err := fs.WalkDir(source, app, func(name string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		values, err := source.Xattrs(name)
		if err != nil {
			return err
		}
		if len(values) != 0 {
			attributes[name] = values
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "GenericFixture.zip")
	testarchive.ZipWithAttributes(t, filename, stage, attributes)
	outputs, err := Prepare(t.Context(), Spec{Application: &Application{Path: app}}, Request{
		Input:     plugin.Artifact{Path: filename, Filename: "GenericFixture.zip", Format: "zip"},
		Workspace: t.TempDir(), DeriveSignature: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := outputs["installer"]
	image, err := diskimage.Open(t.Context(), result.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	if _, err := apple.VerifyAppFS(t.Context(), image, app, signature.Signer{}); err != nil {
		t.Fatal(err)
	}
	for name, attrs := range attributes {
		got, err := image.Xattrs(name)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range attrs {
			if !bytes.Equal(got[key], value) {
				t.Fatalf("attribute %s on %s changed", key, name)
			}
		}
	}
	again, err := Prepare(t.Context(), Spec{Application: &Application{Path: app}}, Request{
		Input: plugin.Artifact{Path: filename, Filename: "GenericFixture.zip", Format: "zip"}, Workspace: t.TempDir(), DeriveSignature: true,
	})
	if err != nil || again["installer"].SHA256 != result.SHA256 {
		t.Fatalf("metadata changed deterministic output: %v", err)
	}
	return result
}

func TestArchiveApplicationPreservesSignedMetadata(t *testing.T) { metadataApplication(t) }

func resourceForkApplication(t *testing.T) plugin.Artifact {
	t.Helper()
	root := applicationFixture(t)
	name := "Example.app/Contents/Info.plist"
	attributes := map[string]map[string][]byte{name: {
		appledouble.ResourceForkName: bytes.Repeat([]byte("fork"), 20000),
		"org.example.attribute":      []byte{0, 1, 2, 255},
		"org.example.empty":          {},
	}}
	filename := filepath.Join(t.TempDir(), "Example.zip")
	testarchive.ZipWithAttributes(t, filename, root, attributes)
	result, err := Prepare(t.Context(), Spec{Application: &Application{Path: "Example.app"}}, Request{
		Input: plugin.Artifact{Path: filename, Filename: "Example.zip", Format: "zip"}, Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	image, err := diskimage.Open(t.Context(), result["installer"].Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	got, err := image.Xattrs(name)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range attributes[name] {
		actual, exists := got[key]
		if !exists || !bytes.Equal(actual, value) {
			t.Fatalf("attribute %s changed", key)
		}
	}
	return result["installer"]
}

func TestArchiveApplicationPreservesResourceFork(t *testing.T) { resourceForkApplication(t) }
