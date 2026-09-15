package apple

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"howett.net/plist"
)

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

func TestAppSymlinkDoesNotClaimResourceSealing(t *testing.T) {
	app := filepath.Join(t.TempDir(), "SignedFixture.app")
	if err := os.CopyFS(app, os.DirFS("testdata/SignedFixture.app")); err != nil {
		t.Fatal(err)
	}
	baseline, err := VerifyApp(t.Context(), app, Policy{RequireIntegrity: true})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(app, "Contents/Resources/link")
	for i, target := range []string{"message.txt", "missing.txt"} {
		if i > 0 {
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		evidence, err := VerifyApp(t.Context(), app, Policy{RequireSignature: true})
		if err != nil || evidence.Integrity.Status != Valid || evidence.Signature.Status != Valid || evidence.Resources.Status != NotRequested || evidence.SubjectSHA256 != baseline.SubjectSHA256 {
			t.Fatalf("unrequested resource changed verification scopes or subject: %+v: %v", evidence, err)
		}
	}
	evidence, err := VerifyApp(t.Context(), app, Policy{RequireResources: true})
	if !errors.Is(err, ErrUnsupported) || evidence.Integrity.Status != Valid || evidence.Resources.Status != Unsupported {
		t.Fatalf("symlink resource sealing was accepted: %+v: %v", evidence, err)
	}
	executable := filepath.Join(app, "Contents/MacOS/fixture")
	data := readTestFile(t, executable)
	// The first architecture's code lies before its large CMS signature region.
	data[16384+4096] ^= 0x40
	writeTestFile(t, executable, data, 0o755)
	if evidence, err := VerifyApp(t.Context(), app, Policy{RequireIntegrity: true}); err == nil || evidence.Integrity.Status != Invalid {
		t.Fatalf("symlink support bypassed executable verification: %+v: %v", evidence, err)
	}
}

func TestAppVerificationRejectsSymlinkedCriticalPaths(t *testing.T) {
	for _, name := range []string{"Contents", "Contents/Info.plist", "Contents/MacOS", "Contents/MacOS/fixture", "Contents/_CodeSignature", "Contents/_CodeSignature/CodeResources"} {
		t.Run(name, func(t *testing.T) {
			app := copyApp(t)
			original := filepath.Join(app, filepath.FromSlash(name))
			moved := original + ".real"
			if err := os.Rename(original, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(moved), original); err != nil {
				t.Fatal(err)
			}
			evidence, err := VerifyApp(t.Context(), app, Policy{RequireIntegrity: true})
			if err == nil || evidence.Integrity.Status == Valid {
				t.Fatalf("critical verification input traversed a symlink: %+v: %v", evidence, err)
			}
		})
	}
}
