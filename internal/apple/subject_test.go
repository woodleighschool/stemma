package apple

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

func TestRootSubjectVerificationUsesLogicalTree(t *testing.T) {
	app, err := filepath.Abs("testdata/NestedFixture.app")
	if err != nil {
		t.Fatal(err)
	}
	want, err := VerifyApp(t.Context(), app, signature.Signer{})
	if err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	if err := archive.Pack(t.Context(), app, &packed); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(t.TempDir(), "tree.tar")
	if err := os.WriteFile(lease, packed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, input := range []plugin.Artifact{
		{Path: app, Filename: "NestedFixture.app", Tree: true},
		{Path: lease, Filename: "NestedFixture.app", Tree: true, Mode: 0o755, Encoding: "tar"},
	} {
		t.Run(input.Encoding, func(t *testing.T) {
			source, err := contents.Open(t.Context(), input, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			got, err := VerifySubject(t.Context(), source, plugin.Subject{Kind: "app", Path: "."}, t.TempDir())
			if closeErr := source.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("root verification changed: %+v, %v; want %+v", got, err, want)
			}
		})
	}
}
