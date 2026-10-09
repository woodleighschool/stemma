package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestCachedOutputsVerifyDescriptorBeforeUsingMetadata(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Import(t.Context(), bytes.NewBufferString("payload"), "")
	if err != nil {
		t.Fatal(err)
	}
	key := config.Fingerprint("descriptor")
	if err := rememberOutputs(t.Context(), store, key, map[string]Prepared{"installer": {Payload: ref, Filename: "output", Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	descriptor, hit := store.Recall(t.Context(), key)
	if !hit {
		t.Fatal("missing descriptor")
	}
	name, err := store.Path(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(data, []byte(`"output"`), []byte(`"forged"`), 1)
	if bytes.Equal(data, changed) {
		t.Fatal("fixture has no filename")
	}
	if err := os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	_, hit, err = recallOutputs(t.Context(), store, key, filepath.Join(t.TempDir(), "cached"))
	if err != nil || hit {
		t.Fatalf("used corrupt descriptor: hit=%v error=%v", hit, err)
	}
}

func TestCachedOutputsVerifyTheConsumedBytes(t *testing.T) {
	for _, tree := range []bool{false, true} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("tree=%v/corrupt=%v", tree, corrupt), func(t *testing.T) {
				store, err := cas.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				directory := t.TempDir()
				file := filepath.Join(directory, "file")
				if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
					t.Fatal(err)
				}
				input := file
				if tree {
					input = directory
				}
				ref, err := importPath(t.Context(), store, input, tree)
				if err != nil {
					t.Fatal(err)
				}
				key := config.Fingerprint("fixture")
				if err := rememberOutputs(t.Context(), store, key, map[string]Prepared{"installer": {Payload: ref, Filename: "output", Tree: tree, Mode: 0o755}}); err != nil {
					t.Fatal(err)
				}
				if corrupt {
					object, err := store.Path(ref)
					if err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(object)
					if err != nil {
						t.Fatal(err)
					}
					data[0] ^= 1
					if err := os.Chmod(object, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(object, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				work := filepath.Join(t.TempDir(), "cached")
				outputs, hit, err := recallOutputs(t.Context(), store, key, work)
				if err != nil || hit == corrupt {
					t.Fatalf("cache hit=%v: %v", hit, err)
				}
				if corrupt {
					if _, err := os.Stat(work); !os.IsNotExist(err) {
						t.Fatalf("retained failed cache lease: %v", err)
					}
					return
				}
				observed, err := digestPath(t.Context(), outputs["installer"].Path, tree)
				if err != nil || observed != ref {
					t.Fatalf("leased payload: %+v %v", observed, err)
				}
			})
		}
	}
}

func TestTreeMaterializationChecksEntireObjectBeforePublication(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := importPath(t.Context(), store, source, true)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Path(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(object, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(object, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("unverified tail after tar EOF"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "output")
	if err := materializeTree(t.Context(), store, ref, target); err == nil {
		t.Fatal("published tree before verifying archive tail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left partial outputs: %v: %v", entries, err)
	}
}

func TestLeaseDigestCoversCanonicalMetadataWithoutImport(t *testing.T) {
	for _, change := range []string{"none", "bytes", "mode", "symlink"} {
		t.Run(change, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			name := filepath.Join(dir, "file")
			if err := os.WriteFile(name, []byte("content"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link")
			if err := os.Symlink("file", link); err != nil {
				t.Skip(err)
			}
			ref, err := importPath(t.Context(), store, dir, true)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			request := plugin.ReconcileRequest[json.RawMessage]{Artifact: plugin.Artifact{Path: dir, Tree: true, SHA256: ref.SHA256, Size: ref.Size, Mode: uint32(info.Mode().Perm())}}
			switch change {
			case "bytes":
				err = os.WriteFile(name, []byte("changed"), 0o600)
			case "mode":
				err = os.Chmod(name, 0o400)
			case "symlink":
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink("missing", link)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = verifyLeases(t.Context(), request)
			if (err == nil) != (change == "none") {
				t.Fatalf("lease check: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(store.Dir, "objects"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("verification imported objects: %v: %v", entries, err)
			}
		})
	}
}
