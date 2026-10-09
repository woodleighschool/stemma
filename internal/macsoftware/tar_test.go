package macsoftware

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/plugin"
)

func TestCanonicalTreePreparationMatchesDirectory(t *testing.T) {
	for _, name := range []string{"SignedFixture.app", "NestedFixture.app", "RequirementFixture.app"} {
		t.Run(name, func(t *testing.T) {
			directory, err := filepath.Abs(filepath.Join("../apple/testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			var packed bytes.Buffer
			if err := archive.Pack(t.Context(), directory, &packed); err != nil {
				t.Fatal(err)
			}
			lease := filepath.Join(t.TempDir(), "tree.tar")
			if err := os.WriteFile(lease, packed.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(directory)
			if err != nil {
				t.Fatal(err)
			}
			var expected plugin.Artifact
			for _, input := range []plugin.Artifact{
				{Path: directory, Filename: name, Tree: true, Mode: uint32(info.Mode().Perm())},
				{Path: lease, Filename: name, Tree: true, Mode: uint32(info.Mode().Perm()), Encoding: "tar"},
			} {
				outputs, err := Prepare(t.Context(), Spec{}, Request{Input: input, Workspace: t.TempDir(), DeriveSignature: true})
				if err != nil {
					t.Fatal(err)
				}
				actual := outputs["installer"]
				actual.Path = ""
				if expected.Filename == "" {
					expected = actual
					continue
				}
				if !reflect.DeepEqual(expected, actual) {
					want, _ := json.Marshal(expected)
					got, _ := json.Marshal(actual)
					t.Fatalf("packed lease changed output or evidence\nwant %s\ngot  %s", want, got)
				}
			}
		})
	}
}
