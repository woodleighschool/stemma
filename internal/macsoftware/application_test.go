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
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
	"github.com/woodleighschool/stemma/plugin"
)

// bundleInputs offers one application as a vendor disk image, as a vendor
// package and as a tree, which preparation places in a disk image.
func bundleInputs(t *testing.T, root string) []plugin.Artifact {
	t.Helper()
	image := filepath.Join(t.TempDir(), "Example.dmg")
	testdiskimage.Write(t, image, root)
	installer := filepath.Join(t.TempDir(), "Example.pkg")
	if err := pkgbuild.Build(t.Context(), filepath.Join(root, "Example.app"), installer, pkgbuild.Options{Identifier: "org.example.app", Version: "1.2", InstallLocation: "/Applications/Example.app", Payload: "."}); err != nil {
		t.Fatal(err)
	}
	return []plugin.Artifact{
		{Path: image, Filename: "Example.dmg", Format: "dmg"},
		{Path: installer, Filename: "Example.pkg", Format: "pkg"},
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

func TestIconExtractsOnlyDeclaredArtwork(t *testing.T) {
	root := applicationFixture(t)
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
	resources := filepath.Join(root, "Example.app/Contents/Resources")
	for _, name := range []string{"AppIcon.icns", "Unrelated.icns", "Assets.car"} {
		if err := os.WriteFile(filepath.Join(resources, name), icns.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	plist := filepath.Join(root, "Example.app/Contents/Info.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("</dict>"), []byte("<key>CFBundleIconName</key><string>AppIcon</string></dict>"), 1)
	if err := os.WriteFile(plist, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Example.app/Contents/PkgInfo"), []byte("APPL????"), 0644); err != nil {
		t.Fatal(err)
	}
	// An executable link must not pull its target or the rest of the bundle into rendering.
	contents := filepath.Join(root, "Example.app/Contents")
	if err := os.MkdirAll(filepath.Join(contents, "Frameworks"), 0755); err != nil {
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
			for _, presentation := range []icon.Presentation{icon.Raw, icon.Glassy} {
				t.Run(string(presentation), func(t *testing.T) {
					workspace := filepath.Join(t.TempDir(), "icon")
					subject, err := Icon(t.Context(), outputs["installer"], workspace, presentation)
					if err != nil {
						t.Fatal(err)
					}
					bundle := filepath.Join(workspace, "expanded/Example.app")
					want := []string{"Contents/Resources/AppIcon.icns"}
					if presentation == icon.Raw {
						if !bytes.Equal(subject.Artwork, artwork.Bytes()) || subject.Path != "" {
							t.Fatal("raw artwork changed")
						}
					} else {
						want = []string{"Contents/Info.plist", "Contents/MacOS/example", "Contents/PkgInfo", "Contents/Resources/AppIcon.icns", "Contents/Resources/Assets.car"}
						if subject.Path != bundle || subject.Artwork != nil {
							t.Fatalf("subject: %+v", subject)
						}
						stub, err := os.ReadFile(filepath.Join(bundle, "Contents/MacOS/example"))
						if err != nil {
							t.Fatal(err)
						}
						if len(stub) != 32 || binary.LittleEndian.Uint32(stub) != 0xfeedfacf {
							t.Fatal("vendor executable was retained instead of the rendering marker")
						}
					}
					if files := bundleFiles(t, bundle); !slices.Equal(files, want) {
						t.Fatalf("extracted %v, want %v", files, want)
					}
				})
			}
		})
	}
	if _, err := Icon(t.Context(), plugin.Artifact{}, t.TempDir(), icon.Glassy); !errors.Is(err, ErrNoApplication) {
		t.Fatalf("missing selection: %v", err)
	}
}

func TestIconDoesNotBroadenMissingArtwork(t *testing.T) {
	for _, name := range []string{"AppIcon", "", "../outside", "[O]ther.icns"} {
		t.Run(name, func(t *testing.T) {
			root := applicationFixture(t)
			plist := filepath.Join(root, "Example.app/Contents/Info.plist")
			data, err := os.ReadFile(plist)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.ReplaceAll(data, []byte("<string>AppIcon</string>"), []byte("<string>"+name+"</string>"))
			if err := os.WriteFile(plist, data, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "Example.app/Contents/Resources/Other.icns"), []byte("not declared"), 0644); err != nil {
				t.Fatal(err)
			}
			input := plugin.Artifact{Path: filepath.Join(root, "Example.app"), Filename: "Example.app", Tree: true}
			outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			for _, presentation := range []icon.Presentation{icon.Raw, icon.Glassy} {
				workspace := filepath.Join(t.TempDir(), "icon")
				_, err := Icon(t.Context(), outputs["installer"], workspace, presentation)
				if name == "../outside" {
					if err == nil {
						t.Fatal("accepted escaping artwork")
					}
				} else if !errors.Is(err, icon.ErrNoArtwork) {
					t.Fatalf("%s missing artwork: %v", presentation, err)
				}
				if err := filepath.WalkDir(workspace, func(_ string, e fs.DirEntry, err error) error {
					if err == nil && e.Name() == "Other.icns" {
						t.Fatal("undeclared artwork was extracted")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestIconWithOnlyAssetCatalog(t *testing.T) {
	for _, declared := range []bool{false, true} {
		root := applicationFixture(t)
		plist := filepath.Join(root, "Example.app/Contents/Info.plist")
		data, err := os.ReadFile(plist)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.ReplaceAll(data, []byte("<key>CFBundleIconFile</key><string>AppIcon</string>"), nil)
		if declared {
			data = bytes.Replace(data, []byte("</dict>"), []byte("<key>CFBundleIconName</key><string>ModernIcon</string></dict>"), 1)
		}
		if err := os.WriteFile(plist, data, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "Example.app/Contents/Resources/Assets.car"), []byte("catalog artwork"), 0644); err != nil {
			t.Fatal(err)
		}
		for _, input := range bundleInputs(t, root) {
			outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			for _, presentation := range []icon.Presentation{icon.Raw, icon.Glassy} {
				subject, err := Icon(t.Context(), outputs["installer"], t.TempDir(), presentation)
				if !declared || presentation == icon.Raw {
					if !errors.Is(err, icon.ErrNoArtwork) {
						t.Fatalf("declared=%v %s: %v", declared, presentation, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"Contents/Info.plist", "Contents/MacOS/example", "Contents/Resources/Assets.car"}
				if files := bundleFiles(t, subject.Path); !slices.Equal(files, want) {
					t.Fatalf("extracted %v, want %v", files, want)
				}
			}
		}
	}
}
