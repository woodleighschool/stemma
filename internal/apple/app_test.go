package apple

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/signature"
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

func TestNestedFixtureMutations(t *testing.T) {
	native := nativeBundleValidity
	portableOnly(t)
	for _, mode := range []string{"portable", "native"} {
		if mode == "native" && native == nil {
			continue
		}
		t.Run(mode, func(t *testing.T) {
			nativeBundleValidity = nil
			if mode == "native" {
				nativeBundleValidity = native
			}
			for _, mutation := range bundleMutations() {
				if mode == "native" && !mutation.parity {
					continue
				}
				t.Run(mutation.name, func(t *testing.T) {
					app := copyFixture(t, "NestedFixture.app")
					mutation.apply(t, app)
					result, err := VerifyApp(t.Context(), app, signature.Signer{})
					if (err == nil) != mutation.accept {
						t.Fatalf("accept = %v, want %v: %+v: %v", err == nil, mutation.accept, result, err)
					}
					if mode == "portable" && errors.Is(err, ErrUnsupported) != mutation.unsupported {
						t.Fatalf("unsupported = %v, want %v: %v", errors.Is(err, ErrUnsupported), mutation.unsupported, err)
					}
					if mutation.accept && (result.Signer != fixtureSigner || result.Name != "Woodleigh School" || result.Authority != "Developer ID Application" || result.Target != "NestedFixture.app") {
						t.Fatalf("wrong result: %+v", result)
					}
				})
			}
		})
	}
}

func TestNativeValidityStillAuthenticatesSigner(t *testing.T) {
	portableOnly(t)
	calls := 0
	nativeBundleValidity = func(context.Context, string) bool {
		calls++
		return true
	}
	app := copyFixture(t, "NestedFixture.app")
	writeTestFile(t, filepath.Join(app, "Contents/Resources/message.txt"), []byte("trusted natively"), 0o644)
	if result, err := VerifyApp(t.Context(), app, signature.Signer{}); err != nil || result.Signer != fixtureSigner {
		t.Fatalf("native validity did not skip the envelope: %+v: %v", result, err)
	}
	corruptExecutable(t, filepath.Join(app, "Contents/MacOS/fixture"), 0)
	if _, err := VerifyApp(t.Context(), app, signature.Signer{}); err == nil {
		t.Fatal("native validity replaced main executable authentication")
	}
	if _, err := VerifyApp(t.Context(), "testdata/Fixture.app", signature.Signer{}); err == nil {
		t.Fatal("native validity established an ad-hoc signer")
	}
	// A bundle the platform rejects is judged by the portable verifier.
	nativeBundleValidity = func(context.Context, string) bool { return false }
	app = copyFixture(t, "NestedFixture.app")
	writeTestFile(t, filepath.Join(app, "Contents/Resources/message.txt"), []byte("judged portably"), 0o644)
	if _, err := VerifyApp(t.Context(), app, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "does not match its seal") {
		t.Fatalf("native rejection did not fall back to the portable verifier: %v", err)
	}
	if calls != 3 {
		t.Fatalf("native verifier consulted %d times", calls)
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
		if err := v.verifyResources(root, data, "MacOS/fixture", "Info.plist"); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s envelope was not rejected as unsupported: %v", name, err)
		}
	}
}

func TestAppVerificationRejectsSymlinkedCriticalPaths(t *testing.T) {
	portableOnly(t)
	for _, name := range []string{"Contents", "Contents/Info.plist", "Contents/MacOS", "Contents/MacOS/fixture", "Contents/_CodeSignature", "Contents/_CodeSignature/CodeResources"} {
		t.Run(name, func(t *testing.T) {
			app := copyFixture(t, "SignedFixture.app")
			original := filepath.Join(app, filepath.FromSlash(name))
			moved := original + ".real"
			if err := os.Rename(original, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(moved), original); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyApp(t.Context(), app, signature.Signer{}); err == nil {
				t.Fatal("critical verification input traversed a symlink")
			}
		})
	}
}

// copyFixture copies a testdata bundle, preserving symlinks and modes.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	source := filepath.Join("testdata", name)
	target := filepath.Join(t.TempDir(), name)
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
