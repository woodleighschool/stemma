package apple

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
)

func scriptBundle(t *testing.T) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Script.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents/MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(app, "Contents/Info.plist"), []byte(`<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.script</string><key>CFBundleExecutable</key><string>script</string><key>CFBundleShortVersionString</key><string>1.0</string></dict></plist>`), 0o644)
	writeTestFile(t, filepath.Join(app, "Contents/MacOS/script"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	return app
}

func TestUnsignedGenericBundleRequiresNoEnvelope(t *testing.T) {
	for _, envelope := range []string{"absent", "empty", "CodeResources", "CodeDirectory"} {
		t.Run(envelope, func(t *testing.T) {
			app := scriptBundle(t)
			if envelope != "absent" {
				dir := filepath.Join(app, "Contents/_CodeSignature")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if envelope != "empty" {
					writeTestFile(t, filepath.Join(dir, envelope), []byte("incomplete"), 0o644)
				}
			}
			_, err := VerifyApp(t.Context(), app, signature.Signer{})
			if err == nil || errors.Is(err, signature.ErrUnsigned) != (envelope == "absent") {
				t.Fatalf("envelope %s: %v", envelope, err)
			}
		})
	}
}

func TestMalformedMachOBundleIsNotUnsignedGenericCode(t *testing.T) {
	app := scriptBundle(t)
	writeTestFile(t, filepath.Join(app, "Contents/MacOS/script"), []byte{0xcf, 0xfa, 0xed, 0xfe}, 0o755)
	if _, err := VerifyApp(t.Context(), app, signature.Signer{}); err == nil || errors.Is(err, signature.ErrUnsigned) {
		t.Fatalf("truncated Mach-O classified as unsigned: %v", err)
	}
}

func TestUnsignedGenericBundleRejectsContainerSignatureAttributes(t *testing.T) {
	app := scriptBundle(t)
	attributes := map[string]map[string][]byte{"Script.app/Contents/MacOS/script": {"com.apple.cs.Unknown": []byte("incomplete")}}
	zip := filepath.Join(t.TempDir(), "Script.zip")
	testarchive.ZipWithAttributes(t, zip, filepath.Dir(app), attributes)
	tree, err := archive.Open(t.Context(), zip, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	dmg := filepath.Join(t.TempDir(), "Script.dmg")
	testdiskimage.WriteXattrs(t, dmg, filepath.Dir(app), attributes)
	image, err := diskimage.Open(t.Context(), dmg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	for name, source := range map[string]fs.ReadLinkFS{"archive": tree, "disk image": image} {
		t.Run(name, func(t *testing.T) {
			_, err := VerifyAppFS(t.Context(), source, "Script.app", signature.Signer{})
			if err == nil || errors.Is(err, signature.ErrUnsigned) || !strings.Contains(err.Error(), "signature attributes") {
				t.Fatalf("container signature attribute presence: %v", err)
			}
		})
	}
}

type executableAttributes struct {
	fs.ReadLinkFS

	values map[string]appledouble.Value
	err    error
}

func (f executableAttributes) XattrValues(name string) (map[string]appledouble.Value, error) {
	if name != "Script.app/Contents/MacOS/script" {
		return nil, errors.New("attributes requested for the wrong executable")
	}
	return f.values, f.err
}

func TestUnsignedGenericBundleChecksAttributeNamesOnly(t *testing.T) {
	app := scriptBundle(t)
	root, err := os.OpenRoot(filepath.Dir(app))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"com.apple.quarantine", "com.apple.cs.CodeDirectory", "com.apple.cs.Unknown"} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []appledouble.Value{nil, unreadAttribute{size: 1 << 40}} {
				fsys := executableAttributes{ReadLinkFS: rootFS(root), values: map[string]appledouble.Value{name: value}}
				_, err := VerifyAppFS(t.Context(), fsys, "Script.app", signature.Signer{})
				if strings.HasPrefix(name, "com.apple.cs.") {
					if err == nil || errors.Is(err, signature.ErrUnsigned) || !strings.Contains(err.Error(), "signature attributes") {
						t.Fatalf("signature attribute presence: %v", err)
					}
				} else if !errors.Is(err, signature.ErrUnsigned) {
					t.Fatalf("unrelated attribute: %v", err)
				}
			}
		})
	}
	want := errors.New("attribute read failed")
	_, err = VerifyAppFS(t.Context(), executableAttributes{ReadLinkFS: rootFS(root), err: want}, "Script.app", signature.Signer{})
	if !errors.Is(err, want) || errors.Is(err, signature.ErrUnsigned) {
		t.Fatalf("attribute read failure: %v", err)
	}
}
