package apple

import (
	"errors"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
)

type unreadAttribute struct{ size int64 }

func (v unreadAttribute) Size() int64 { return v.size }
func (unreadAttribute) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("attribute must not be read")
}

type selectiveAttributes struct {
	*diskimage.Image

	name  string
	value appledouble.Value
}

func (s selectiveAttributes) XattrValues(name string) (map[string]appledouble.Value, error) {
	values, err := s.Image.XattrValues(name)
	if err == nil {
		values[s.name] = s.value
	}
	return values, err
}

func TestGenericSignatureReadsOnlyBoundedSignatureAttributes(t *testing.T) {
	image, err := diskimage.Open(t.Context(), "testdata/generic.dmg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	for _, test := range []struct {
		name, rejection string
		size            int64
	}{
		{appledouble.ResourceForkName, "", 1 << 40},
		{"com.apple.cs.CodeSignature", "exceeds its size limit", maxSignature + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := VerifyAppFS(t.Context(), selectiveAttributes{image, test.name, unreadAttribute{test.size}}, genericApp, signature.Signer{})
			if test.rejection == "" && err != nil || test.rejection != "" && (err == nil || !strings.Contains(err.Error(), test.rejection)) {
				t.Fatalf("got %v, want %q", err, test.rejection)
			}
		})
	}
}

const (
	genericApp     = "GenericFixture.app"
	genericMessage = genericApp + "/Contents/MacOS/share/message.txt"
	// genericOther also carries an alternate SHA-256 CodeDirectory.
	genericOther = genericApp + "/Contents/MacOS/share/other.txt"
)

// TestGenericCodeInImages verifies generic code where hdiutil laid out its
// extended attributes.
func TestGenericCodeInImages(t *testing.T) {
	for _, name := range []string{"generic.dmg", "generic-apfs.dmg"} {
		t.Run(name, func(t *testing.T) {
			image, err := diskimage.Open(t.Context(), filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = image.Close() }()
			result, err := VerifyAppFS(t.Context(), image, genericApp, signature.Signer{})
			if err != nil || result.Signer != fixtureSigner || len(result.Replaced) != 0 {
				t.Fatalf("generic code in %s: %+v: %v", name, result, err)
			}
		})
	}
}

func TestGenericCodeMutations(t *testing.T) {
	source, err := diskimage.Open(t.Context(), "testdata/generic.dmg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	tree := filepath.Join(t.TempDir(), "tree")
	app, err := source.Extract(t.Context(), tree, genericApp, nil)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]map[string][]byte{}
	for _, name := range []string{genericMessage, genericOther} {
		if original[name], err = source.Xattrs(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := VerifyApp(t.Context(), app, signature.Signer{}); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "extended attributes") {
		t.Fatalf("generic code verified without its extended attributes: %v", err)
	}
	mutations := []struct {
		name, rejection string
		change          func(t *testing.T, dir string, xattrs map[string]map[string][]byte)
	}{
		{"intact", "", func(*testing.T, string, map[string]map[string][]byte) {}},
		{"content changed", "hash mismatch", func(t *testing.T, dir string, _ map[string]map[string][]byte) {
			t.Helper()
			writeTestFile(t, filepath.Join(dir, genericMessage), []byte("sealed massage\n"), 0o644)
		}},
		{"length changed", "code limit differs", func(t *testing.T, dir string, _ map[string]map[string][]byte) {
			t.Helper()
			writeTestFile(t, filepath.Join(dir, genericMessage), []byte("sealed message, changed\n"), 0o644)
		}},
		{"signature removed", "unsigned nested code", func(_ *testing.T, _ string, xattrs map[string]map[string][]byte) {
			delete(xattrs, genericMessage)
		}},
		{"signature of another file", "code limit differs", func(_ *testing.T, _ string, xattrs map[string]map[string][]byte) {
			xattrs[genericMessage] = xattrs[genericOther]
		}},
		{"alternate CodeDirectory removed", "hash agility", func(_ *testing.T, _ string, xattrs map[string]map[string][]byte) {
			delete(xattrs[genericOther], "com.apple.cs.CodeRequirements-1")
		}},
		{"unknown signature attribute", "signature attribute com.apple.cs.CodeFuture", func(_ *testing.T, _ string, xattrs map[string]map[string][]byte) {
			xattrs[genericMessage]["com.apple.cs.CodeFuture"] = []byte{0xfa, 0xde, 0x0c, 0x09, 0, 0, 0, 8}
		}},
	}
	// One image holds every case, each under its index.
	stage := t.TempDir()
	staged := map[string]map[string][]byte{}
	for i, mutation := range mutations {
		dir := filepath.Join(stage, strconv.Itoa(i))
		if err := os.CopyFS(dir, os.DirFS(tree)); err != nil {
			t.Fatal(err)
		}
		xattrs := map[string]map[string][]byte{}
		for name, values := range original {
			xattrs[name] = maps.Clone(values)
		}
		mutation.change(t, dir, xattrs)
		for name, values := range xattrs {
			staged[path.Join(strconv.Itoa(i), name)] = values
		}
	}
	dmg := filepath.Join(t.TempDir(), "mutations.dmg")
	testdiskimage.WriteXattrs(t, dmg, stage, staged)
	image, err := diskimage.Open(t.Context(), dmg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	for i, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			_, err := VerifyAppFS(t.Context(), image, path.Join(strconv.Itoa(i), genericApp), signature.Signer{})
			if mutation.rejection == "" && err != nil || mutation.rejection != "" && (err == nil || !strings.Contains(err.Error(), mutation.rejection)) {
				t.Fatalf("got %v, want %q", err, mutation.rejection)
			}
		})
	}
}
