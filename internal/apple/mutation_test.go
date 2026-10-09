package apple

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-macos-pkg/pkg/pkgsign"
	"github.com/woodleighschool/stemma/internal/signature"
)

// mutateReadFS preserves the filesystem contract while changing an entry after
// its bytes have been read, before the verifier releases the held file.
type mutateReadFS struct {
	fs.ReadLinkFS

	name   string
	mutate func()
}

func (s mutateReadFS) Open(name string) (fs.File, error) {
	f, err := s.ReadLinkFS.Open(name)
	if err == nil && name == s.name {
		return &mutateReadFile{File: f.(*os.File), mutate: s.mutate}, nil
	}
	return f, err
}

type mutateReadFile struct {
	*os.File

	mutate func()
}

func (f *mutateReadFile) ReadAt(p []byte, offset int64) (int, error) {
	n, err := f.File.ReadAt(p, offset)
	if f.mutate != nil {
		f.mutate()
		f.mutate = nil
	}
	return n, err
}

func TestMutationCannotBecomeUnsigned(t *testing.T) {
	app := scriptBundle(t)
	fsys := mutateReadFS{
		ReadLinkFS: os.DirFS(filepath.Dir(app)).(fs.ReadLinkFS),
		name:       "Script.app/Contents/MacOS/script",
		mutate: func() {
			writeTestFile(t, filepath.Join(app, "Contents/MacOS/script"), []byte("modified after read"), 0o755)
		},
	}
	_, err := VerifyAppFS(t.Context(), executableAttributes{ReadLinkFS: fsys}, "Script.app", signature.Signer{})
	if err == nil || errors.Is(err, signature.ErrUnsigned) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("mutated executable treated as unsigned: %v", err)
	}
}

func TestMutationInvalidatesVerifiedExecutable(t *testing.T) {
	app := copyFixture(t, "SignedFixture.app")
	fsys := mutateReadFS{
		ReadLinkFS: os.DirFS(filepath.Dir(app)).(fs.ReadLinkFS),
		name:       "SignedFixture.app/Contents/MacOS/fixture",
		mutate: func() {
			name := filepath.Join(app, "Contents/MacOS/fixture")
			if err := os.Rename(name, name+".old"); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, name, readTestFile(t, name+".old"), 0o755)
		},
	}
	if _, err := VerifyAppFS(t.Context(), fsys, "SignedFixture.app", signature.Signer{}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal("accepted replaced executable")
	}
}

func TestMutationInvalidatesNestedExecutable(t *testing.T) {
	app := copyFixture(t, "NestedFixture.app")
	dir := filepath.Join(app, "Contents/MacOS")
	v := bundleVerifier{ctx: t.Context(), roots: pkgsign.AppleRootCertificates()}
	fsys := mutateReadFS{
		ReadLinkFS: os.DirFS(dir).(fs.ReadLinkFS),
		name:       "helper",
		mutate: func() {
			name := filepath.Join(dir, "helper")
			if err := os.Rename(name, name+".old"); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, name, readTestFile(t, name+".old"), 0o755)
		},
	}
	if _, err := v.verifyNestedCode(fsys.ReadLinkFS, "", "helper", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := v.verifyNestedCode(fsys, "", "helper", false, nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("accepted replaced nested executable: %v", err)
	}
}
