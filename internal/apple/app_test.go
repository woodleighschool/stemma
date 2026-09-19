package apple

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testdiskimage"
	"howett.net/plist"
)

const fixtureSigner = "apple:developer-id:SMLKBTR495"

func TestParseAppInfoMinimumSystemVersion(t *testing.T) {
	for _, tt := range []struct {
		name    string
		scalar  string
		byArch  map[string]string
		want    string
		wantErr bool
	}{
		{name: "scalar-precedence", scalar: "10.15", byArch: map[string]string{"arm64": "13.0", "x86_64": "11.0"}, want: "10.15"},
		{name: "numeric-maximum", byArch: map[string]string{"arm64": "11.0", "x86_64": "9.0"}, want: "11.0"},
		{name: "patch-maximum", byArch: map[string]string{"arm64": "13.0.1", "x86_64": "13.0"}, want: "13.0.1"},
		{name: "equivalent-versions", byArch: map[string]string{"arm64": "13.0", "x86_64": "13.0.0"}, want: "13.0"},
		{name: "absent"},
		{name: "invalid", byArch: map[string]string{"arm64": "unknown"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := plist.Marshal(map[string]any{"CFBundleIdentifier": "org.example.fixture", "CFBundleExecutable": "fixture", "LSMinimumSystemVersion": tt.scalar, "LSMinimumSystemVersionByArchitecture": tt.byArch}, plist.XMLFormat)
			if err != nil {
				t.Fatal(err)
			}
			facts, err := ParseAppInfo(data)
			if (err != nil) != tt.wantErr || (!tt.wantErr && facts.MinimumOS != tt.want) {
				t.Fatalf("minimum OS = %q, err = %v; want %q, err = %v", facts.MinimumOS, err, tt.want, tt.wantErr)
			}
		})
	}
}

// bundleMutation changes a copy of NestedFixture.app. accept is Stemma's
// verdict; parity marks cases where codesign --verify --strict --deep must agree.
type bundleMutation struct {
	name        string
	apply       func(*testing.T, string)
	accept      bool
	unsupported bool
	parity      bool
}

func bundleMutations() []bundleMutation {
	write := func(name string, data []byte) func(*testing.T, string) {
		return func(t *testing.T, app string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(app, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(app, name), data, 0o644)
		}
	}
	remove := func(name string) func(*testing.T, string) {
		return func(t *testing.T, app string) {
			t.Helper()
			if err := os.RemoveAll(filepath.Join(app, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	replace := func(name, with string) func(*testing.T, string) {
		return func(t *testing.T, app string) {
			t.Helper()
			writeTestFile(t, filepath.Join(app, name), readTestFile(t, filepath.Join(app, with)), 0o755)
		}
	}
	corrupt := func(name string, arch int) func(*testing.T, string) {
		return func(t *testing.T, app string) {
			t.Helper()
			corruptExecutable(t, filepath.Join(app, name), arch)
		}
	}
	symlink := func(name, target string) func(*testing.T, string) {
		return func(t *testing.T, app string) {
			t.Helper()
			link := filepath.Join(app, name)
			_ = os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		}
	}
	return []bundleMutation{
		{"intact", func(*testing.T, string) {}, true, false, true},
		{"optional localization removed", remove("Contents/Resources/en.lproj/Localizable.strings"), true, false, true},
		{"unsealed pkginfo changed", write("Contents/PkgInfo", []byte("BNDL????")), true, false, true},
		{"finder metadata added", write("Contents/Resources/.DS_Store", []byte("metadata")), true, false, true},
		{"resource modified", write("Contents/Resources/message.txt", []byte("modified")), false, false, true},
		{"resource removed", remove("Contents/Resources/message.txt"), false, false, true},
		{"resource added", write("Contents/Resources/extra.txt", []byte("unsealed")), false, false, true},
		{"framework resource added", write("Contents/Frameworks/Nested.framework/Versions/A/extra.txt", []byte("unsealed")), false, false, true},
		{"bundle root file added", write("extra", []byte("unsealed")), false, false, true},
		{"symlink retargeted", symlink("Contents/Resources/link", "missing.txt"), false, false, true},
		{"symlink replaced by file", func(t *testing.T, app string) {
			t.Helper()
			_ = os.Remove(filepath.Join(app, "Contents/Resources/link"))
			writeTestFile(t, filepath.Join(app, "Contents/Resources/link"), []byte("sealed message\n"), 0o644)
		}, false, false, true},
		{"resource replaced by symlink", func(t *testing.T, app string) {
			t.Helper()
			_ = os.Remove(filepath.Join(app, "Contents/Resources/message.txt"))
			if err := os.Symlink("link", filepath.Join(app, "Contents/Resources/message.txt")); err != nil {
				t.Fatal(err)
			}
		}, false, false, true},
		{"info modified", func(t *testing.T, app string) {
			t.Helper()
			f := filepath.Join(app, "Contents/Info.plist")
			writeTestFile(t, f, []byte(strings.Replace(string(readTestFile(t, f)), "<string>1.0</string>", "<string>9.0</string>", 1)), 0o644)
		}, false, false, true},
		{"envelope modified", func(t *testing.T, app string) {
			t.Helper()
			f := filepath.Join(app, "Contents/_CodeSignature/CodeResources")
			writeTestFile(t, f, append(readTestFile(t, f), '\n'), 0o644)
		}, false, false, true},
		{"signature removed", remove("Contents/_CodeSignature"), false, false, true},
		{"executable modified first architecture", corrupt("Contents/MacOS/fixture", 0), false, false, true},
		{"executable modified second architecture", corrupt("Contents/MacOS/fixture", 1), false, false, true},
		{"helper modified", corrupt("Contents/MacOS/helper", 1), false, false, true},
		{"framework binary modified", corrupt("Contents/Frameworks/Nested.framework/Versions/A/Nested", 0), false, false, true},
		{"shallow framework binary modified", corrupt("Contents/Frameworks/Shallow.framework/Shallow", 1), false, false, true},
		{"nested app binary modified", corrupt("Contents/Helpers/Helper.app/Contents/MacOS/Helper", 0), false, false, true},
		{"nested app info modified", func(t *testing.T, app string) {
			t.Helper()
			f := filepath.Join(app, "Contents/Helpers/Helper.app/Contents/Info.plist")
			writeTestFile(t, f, []byte(strings.Replace(string(readTestFile(t, f)), "<string>1.0</string>", "<string>9.0</string>", 1)), 0o644)
		}, false, false, true},
		// Same team, different code: a bundle binary is bound to its own Info.plist,
		// a bundle to the exact sealed cdhash.
		{"framework binary swapped", replace("Contents/Frameworks/Nested.framework/Versions/A/Nested", "Contents/Frameworks/Shallow.framework/Shallow"), false, false, false},
		{"helper swapped", replace("Contents/MacOS/helper", "Contents/Helpers/Helper.app/Contents/MacOS/Helper"), false, true, false},
		{"nested app swapped", func(t *testing.T, app string) {
			t.Helper()
			helper := filepath.Join(app, "Contents/Helpers/Helper.app")
			if err := os.RemoveAll(helper); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(copyFixture(t, "NestedFixture.app"), helper); err != nil {
				t.Fatal(err)
			}
		}, false, false, false},
		{"framework version added", write("Contents/Frameworks/Nested.framework/Versions/B/extra", []byte("other version")), false, true, false},
		{"framework current retargeted", symlink("Contents/Frameworks/Nested.framework/Versions/Current", "B"), false, true, false},
		{"framework root file added", write("Contents/Frameworks/Nested.framework/extra", []byte("unsealed")), false, true, true},
	}
}

// TestNestedFixtureMutations judges every case on disk.
func TestNestedFixtureMutations(t *testing.T) {
	for _, mutation := range bundleMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			app := copyFixture(t, "NestedFixture.app")
			mutation.apply(t, app)
			result, err := VerifyApp(t.Context(), app, signature.Signer{})
			checkMutation(t, mutation, result, err)
		})
	}
}

// TestNestedFixtureMutationsInImage judges the same cases where they lie in a
// disk image. One image holds them all.
func TestNestedFixtureMutationsInImage(t *testing.T) {
	mutations := bundleMutations()
	stage := t.TempDir()
	for i, mutation := range mutations {
		mutation.apply(t, copyFixtureTo(t, filepath.Join(stage, strconv.Itoa(i)), "NestedFixture.app"))
	}
	image := openImage(t, stage)
	for i, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			result, err := VerifyAppFS(t.Context(), image, path.Join(strconv.Itoa(i), "NestedFixture.app"), signature.Signer{})
			checkMutation(t, mutation, result, err)
		})
	}
}

func checkMutation(t *testing.T, mutation bundleMutation, result signature.Result, err error) {
	t.Helper()
	if (err == nil) != mutation.accept {
		t.Fatalf("accept = %v, want %v: %+v: %v", err == nil, mutation.accept, result, err)
	}
	if errors.Is(err, ErrUnsupported) != mutation.unsupported {
		t.Fatalf("unsupported = %v, want %v: %v", errors.Is(err, ErrUnsupported), mutation.unsupported, err)
	}
	if mutation.accept && (result.Signer != fixtureSigner || result.Name != "Woodleigh School" || result.Authority != "Developer ID Application" || result.Target != "NestedFixture.app") {
		t.Fatalf("wrong result: %+v", result)
	}
}

// TestCodesignAgreesWithMutations uses Apple's verifier as an oracle for the
// shapes Stemma claims to support.
func TestCodesignAgreesWithMutations(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign is only available on macOS")
	}
	for _, mutation := range bundleMutations() {
		if !mutation.parity {
			continue
		}
		t.Run(mutation.name, func(t *testing.T) {
			app := copyFixture(t, "NestedFixture.app")
			mutation.apply(t, app)
			output, err := exec.CommandContext(t.Context(), "/usr/bin/codesign", "--verify", "--strict", "--deep", app).CombinedOutput()
			if (err == nil) != mutation.accept {
				t.Fatalf("codesign accept = %v, want %v: %s", err == nil, mutation.accept, output)
			}
		})
	}
}

func TestAppSignerMismatchReportsObservedSigner(t *testing.T) {
	result, err := VerifyApp(t.Context(), "testdata/NestedFixture.app", signature.Signer{Scheme: signature.AppleDeveloperID, Value: "AAAAAAAAAA"})
	if !errors.Is(err, signature.ErrMismatch) || result.Signer != fixtureSigner {
		t.Fatalf("mismatch: %+v: %v", result, err)
	}
	if _, err := VerifyApp(t.Context(), "testdata/NestedFixture.app", signature.Signer{Scheme: signature.AppleDeveloperID, Value: "SMLKBTR495"}); err != nil {
		t.Fatal(err)
	}
}

func TestAppRequiresDeveloperIDSignature(t *testing.T) {
	if _, err := VerifyApp(t.Context(), "testdata/Fixture.app", signature.Signer{}); err == nil || !strings.Contains(err.Error(), "no CMS signature") {
		t.Fatalf("ad-hoc application accepted: %v", err)
	}
}

func TestResourceEnvelopeRequiresFiles2(t *testing.T) {
	app := copyFixture(t, "SignedFixture.app")
	root, err := os.OpenRoot(filepath.Join(app, "Contents"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	contents := rootFS(root)
	v := &bundleVerifier{ctx: t.Context(), buffer: make([]byte, 4096)}
	for name, seals := range map[string]any{
		"legacy": map[string]any{"files": map[string]any{"Resources/message.txt": make([]byte, 20)}},
		"sha1":   map[string]any{"files2": map[string]any{"Resources/message.txt": map[string]any{"hash": make([]byte, 20)}}},
		"field":  map[string]any{"files2": map[string]any{"Resources/message.txt": map[string]any{"hash2": make([]byte, 32), "weight": 10}}},
	} {
		data, err := plist.Marshal(seals, plist.XMLFormat)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.verifyResources(contents, data, "MacOS/fixture", "Info.plist"); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s envelope was not rejected as unsupported: %v", name, err)
		}
	}
}

func TestAppVerificationRejectsSymlinkedCriticalPaths(t *testing.T) {
	names := []string{"Contents", "Contents/Info.plist", "Contents/MacOS", "Contents/MacOS/fixture", "Contents/_CodeSignature", "Contents/_CodeSignature/CodeResources"}
	stage := t.TempDir()
	apps := make([]string, len(names))
	for i, name := range names {
		apps[i] = copyFixtureTo(t, filepath.Join(stage, strconv.Itoa(i)), "SignedFixture.app")
		original := filepath.Join(apps[i], filepath.FromSlash(name))
		moved := original + ".real"
		if err := os.Rename(original, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Base(moved), original); err != nil {
			t.Fatal(err)
		}
	}
	image := openImage(t, stage)
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyApp(t.Context(), apps[i], signature.Signer{}); err == nil {
				t.Fatal("critical verification input traversed a symlink on disk")
			}
			if _, err := VerifyAppFS(t.Context(), image, path.Join(strconv.Itoa(i), "SignedFixture.app"), signature.Signer{}); err == nil {
				t.Fatal("critical verification input traversed a symlink in a disk image")
			}
		})
	}
}

func TestAppInFilesystemRejectsSymlinkedLocation(t *testing.T) {
	app := copyFixture(t, "SignedFixture.app")
	if err := os.Symlink("SignedFixture.app", filepath.Join(filepath.Dir(app), "Alias.app")); err != nil {
		t.Fatal(err)
	}
	image := openImage(t, filepath.Dir(app))
	if _, err := VerifyAppFS(t.Context(), image, "SignedFixture.app", signature.Signer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAppFS(t.Context(), image, "Alias.app", signature.Signer{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("bundle reached through a symlink: %v", err)
	}
}

// sequentialFS hides random access from the files of a filesystem.
type sequentialFS struct{ fs.ReadLinkFS }

func (s sequentialFS) Open(name string) (fs.File, error) {
	f, err := s.ReadLinkFS.Open(name)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err != nil || info.IsDir() {
		return f, err
	}
	return struct{ fs.File }{f}, nil
}

func TestAppInFilesystemRequiresRandomAccess(t *testing.T) {
	root, err := os.OpenRoot("testdata")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fsys := rootFS(root)
	if _, err := VerifyAppFS(t.Context(), fsys, "SignedFixture.app", signature.Signer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAppFS(t.Context(), sequentialFS{fsys}, "SignedFixture.app", signature.Signer{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("files without random access were read: %v", err)
	}
}

// openImage writes a directory's children into a disk image and opens it, so
// bundles are judged where none lies on disk.
func openImage(t *testing.T, dir string) *diskimage.Image {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture writer stores symlink targets with the host's separator")
	}
	name := filepath.Join(t.TempDir(), "fixture.dmg")
	if err := testdiskimage.Write(name, dir); err != nil {
		t.Fatal(err)
	}
	image, err := diskimage.Open(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = image.Close() })
	return image
}

// copyFixture copies a testdata bundle, preserving symlinks and modes.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	return copyFixtureTo(t, t.TempDir(), name)
}

func copyFixtureTo(t *testing.T, dir, name string) string {
	t.Helper()
	source := filepath.Join("testdata", name)
	target := filepath.Join(dir, name)
	err := filepath.WalkDir(source, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(current)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		case entry.IsDir():
			return os.MkdirAll(destination, 0o755)
		default:
			data, err := os.ReadFile(current)
			if err != nil {
				return err
			}
			return os.WriteFile(destination, data, info.Mode().Perm())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return target
}
