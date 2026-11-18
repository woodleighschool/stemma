package apple

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
)

func TestFileBackedBundleSignatures(t *testing.T) {
	cert, err := x509.ParseCertificate(readTestFile(t, "testdata/codeless-test.cer"))
	if err != nil {
		t.Fatal(err)
	}
	const version = "Contents/Frameworks/Versioned.framework/Versions/A/"
	const metadata = version + "_CodeSignature/"
	for _, test := range []struct {
		name, file string
		change     func(*testing.T, string)
		want       string
	}{
		{name: "intact"},
		{"plist", version + "Resources/Info.plist", func(t *testing.T, file string) {
			t.Helper()
			data := readTestFile(t, file)
			writeTestFile(t, file, bytes.Replace(data, []byte("org.example.versioned"), []byte("org.example.different"), 1), 0o644)
		}, "code page"},
		{"main script", "Contents/Frameworks/Shallow.framework/Shallow", flipLastByte, "code page"},
		{"missing declared script", "Contents/Frameworks/Shallow.framework/Shallow", func(t *testing.T, file string) {
			t.Helper()
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}, `bundle main file "Shallow"`},
		{"primary directory", metadata + "CodeDirectory", flipLastByte, "code page"},
		{"alternate directory", metadata + "CodeRequirements-1", flipLastByte, "code page"},
		{"requirements", metadata + "CodeRequirements", flipLastByte, "special slot"},
		{"CMS", metadata + "CodeSignature", func(t *testing.T, file string) {
			t.Helper()
			other := "testdata/Codeless.bundle/Contents/Frameworks/Shallow.framework/_CodeSignature/CodeSignature"
			writeTestFile(t, file, readTestFile(t, other), 0o644)
		}, "CMS"},
		{"resource envelope", metadata + "CodeResources", flipLastByte, "special slot"},
		{"sealed script", version + "Resources/Helper", flipLastByte, "does not match its seal"},
		{"unknown component", metadata + "Extra", func(t *testing.T, file string) {
			t.Helper()
			writeTestFile(t, file, []byte("extra"), 0o644)
		}, "unexpected signature file"},
		{"unused component", metadata + "CodeEntitlements", func(t *testing.T, file string) {
			t.Helper()
			writeTestFile(t, file, readTestFile(t, filepath.Join(filepath.Dir(file), "CodeRequirements")), 0o644)
		}, "required external slot"},
		{"symlink component", metadata + "CodeRequirements", func(t *testing.T, file string) {
			t.Helper()
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("CodeDirectory", file); err != nil {
				t.Fatal(err)
			}
		}, "unexpected signature file"},
		{"missing primary directory", metadata + "CodeDirectory", func(t *testing.T, file string) {
			t.Helper()
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}, "missing primary CodeDirectory"},
		{"alternate directory gap", metadata + "CodeRequirements-1", func(t *testing.T, file string) {
			t.Helper()
			if err := os.Rename(file, filepath.Join(filepath.Dir(file), "CodeRequirements-2")); err != nil {
				t.Fatal(err)
			}
		}, "missing alternate CodeDirectory"},
		{"missing CMS", metadata + "CodeSignature", func(t *testing.T, file string) {
			t.Helper()
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}, "no CMS signature"},
		{"conflicting executable", version + "Versioned", func(t *testing.T, file string) {
			t.Helper()
			writeTestFile(t, file, readTestFile(t, "testdata/SignedFixture.app/Contents/MacOS/fixture"), 0o755)
		}, "unexpected signature files"},
		{"parent seals different framework", "Contents/Frameworks/Shallow.framework", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(filepath.Dir(dir), "Versioned.framework/Versions/A")
			if err := os.CopyFS(dir, os.DirFS(source)); err != nil {
				t.Fatal(err)
			}
		}, "does not match its sealed cdhash"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := copyFixture(t, "Codeless.bundle")
			if test.change != nil {
				test.change(t, filepath.Join(app, test.file))
			}
			root, err := os.OpenRoot(app)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			verifier := bundleVerifier{ctx: t.Context(), roots: []*x509.Certificate{cert}}
			identity, err := verifier.verifyContents(rootFS(root), "", filepath.Base(app))
			if test.want == "" {
				if err != nil || identity.identifier != "org.example.host" || identity.team != "TESTSIGNER" || len(identity.cdhashes) != 2 || len(verifier.replaced) != 0 {
					t.Fatalf("identity = %+v, replacements = %v, error = %v", identity, verifier.replaced, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) || errors.Is(err, signature.ErrUnsigned) {
				t.Errorf("got %v, want %q", err, test.want)
			}
			// codesign permits some known but unused metadata. Stemma requires a seal for each supplied component.
			if runtime.GOOS == "darwin" && test.name != "unused component" {
				out, err := exec.CommandContext(t.Context(), "/usr/bin/codesign", "--verify", "--deep", "--strict", app).CombinedOutput()
				if (err == nil) != (test.want == "") {
					t.Errorf("codesign: %v: %s", err, out)
				}
			}
		})
	}
}

func flipLastByte(t *testing.T, file string) {
	t.Helper()
	data := readTestFile(t, file)
	data[len(data)-1] ^= 1
	writeTestFile(t, file, data, 0o644)
}

func TestCodelessBundleRejectsMixedSignatureStorage(t *testing.T) {
	app := copyFixture(t, "Codeless.bundle")
	attributes := map[string]map[string][]byte{"Codeless.bundle/Contents/Info.plist": {
		"com.apple.cs.CodeDirectory": readTestFile(t, filepath.Join(app, "Contents/_CodeSignature/CodeDirectory")),
	}}
	dmg := filepath.Join(t.TempDir(), "mixed.dmg")
	testdiskimage.WriteXattrs(t, dmg, filepath.Dir(app), attributes)
	image, err := diskimage.Open(t.Context(), dmg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	_, err = VerifyAppFS(t.Context(), image, "Codeless.bundle", signature.Signer{})
	if err == nil || !strings.Contains(err.Error(), "signature attributes") || errors.Is(err, signature.ErrUnsigned) {
		t.Fatalf("mixed signature storage: %v", err)
	}
}

func TestUnityHubCodelessFramework(t *testing.T) {
	dmg := os.Getenv("STEMMA_TEST_UNITY_HUB_DMG")
	if dmg == "" {
		t.Skip("set STEMMA_TEST_UNITY_HUB_DMG to the Unity Hub 4.0.1 arm64 disk image")
	}
	file, err := os.Open(dmg)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := fileDigest(t.Context(), file, nil)
	if err = errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	if sum != "38604d555a6cb8faf01a1cc80d647bfb381a57387611923d83f5b3b2b519f293" {
		t.Fatalf("unexpected Unity Hub 4.0.1 arm64 disk image SHA-256: %s", sum)
	}
	image, err := diskimage.Open(t.Context(), dmg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	want := signature.Signer{Scheme: signature.AppleDeveloperID, Value: "9QW8UQUTAA"}
	result, err := VerifyAppFS(t.Context(), image, "Unity Hub.app", want)
	if err != nil || result.Signer != want.String() {
		t.Fatalf("signature = %+v, error = %v", result, err)
	}
}
