package munkirepo_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/munkirepo"
	"github.com/woodleighschool/stemma/plugin"
)

func TestRetentionPreservesManifestPinsAndSharedInstallerReferences(t *testing.T) {
	root, request := repositoryRequest(t, "Editor.pkg", `{}`)
	paths := map[string]string{}
	publish := func(version string) {
		data := []byte("synthetic installer " + version)
		digest := sha256.Sum256(data)
		if err := os.WriteFile(request.Artifact.Path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		request.Artifact.Version, request.Artifact.SHA256, request.Artifact.Size = version, hex.EncodeToString(digest[:]), int64(len(data))
		paths[version] = apply(t, root, &request)
	}
	for _, version := range []string{"1", "2", "3"} {
		publish(version)
	}
	pinned := readNative[map[string]any](t, paths["1"])
	writeNative(t, filepath.Join(root, "manifests", "client"), map[string]any{"conditional_items": []any{map[string]any{"condition": "true == true", "managed_installs": []string{"App-1"}}}})
	shared := readNative[map[string]any](t, paths["2"])
	writeNative(t, filepath.Join(root, "pkgsinfo", "foreign.plist"), map[string]any{"name": "Foreign", "version": "1", "installer_item_location": shared["installer_item_location"]})
	request.Metadata = json.RawMessage(`{"retention":{"keep":2},"pkginfo":{}}`)
	publish("4")
	if _, err := os.Stat(paths["1"]); err != nil {
		t.Fatal("pinned publication was pruned")
	}
	if _, err := os.Stat(paths["2"]); !os.IsNotExist(err) {
		t.Fatal("unreferenced old publication was retained")
	}
	for _, document := range []map[string]any{pinned, shared} {
		location := document["installer_item_location"].(string)
		if _, err := os.Stat(filepath.Join(root, "pkgs", filepath.FromSlash(location))); err != nil {
			t.Fatal("referenced installer bytes were pruned")
		}
	}
	for _, version := range []string{"3", "4"} {
		if _, err := os.Stat(paths[version]); err != nil {
			t.Fatal("recent publication was pruned")
		}
	}
	assertConverged(t, request)
}

func TestRetentionPlanIsReadOnlyAndBindingLossDoesNotReclaim(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{}`)
	first := apply(t, root, &request)
	var before, after struct {
		Publications plugin.Publications `json:"publications"`
	}
	if err := json.Unmarshal(request.Binding, &before); err != nil {
		t.Fatal(err)
	}
	request.Artifact.Version = "2"
	apply(t, root, &request)
	if err := json.Unmarshal(request.Binding, &after); err != nil {
		t.Fatal(err)
	}
	if before.Publications.Sequence != after.Publications.Sequence || len(after.Publications.Order) != 1 {
		t.Fatal("metadata-only native version edit advanced payload publication order")
	}
	request.Metadata = json.RawMessage(`{"retention":{"keep":1},"pkginfo":{}}`)
	request.Method = "plan"
	response, err := munkirepo.Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range response.Changes {
		if change.Kind == "retention" {
			found = true
		}
	}
	if !found {
		t.Fatal("plan omitted retention deletion")
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatal("plan deleted a publication")
	}
	request.Binding = nil
	request.Method = "apply"
	if _, err := munkirepo.Handle(t.Context(), request); err == nil {
		t.Fatal("reclaimed owned path from its marker after binding loss")
	}
}

func TestDisappearingDerivedMetadataIsCleared(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{}`)
	request.Facts = plugin.Facts{Subjects: []plugin.Subject{{Kind: "app", InstalledPath: "/Applications/App.app", App: &plugin.AppFacts{BundleID: "example.app", Version: "1", MinimumOS: "13.0"}}}}
	file := apply(t, root, &request)
	request.Facts.Subjects[0].App.MinimumOS = ""
	apply(t, root, &request)
	if value, exists := readNative[map[string]any](t, file)["minimum_os_version"]; exists {
		t.Fatalf("retained stale derived field: %#v", value)
	}
}

func TestRetentionRejectsUnreadableReferenceDocuments(t *testing.T) {
	for _, kind := range []string{"symlink", "directory_symlink", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			root, request := repositoryRequest(t, "App.pkg", `{}`)
			first := apply(t, root, &request)
			request.Artifact.Version = "2"
			apply(t, root, &request)
			filename := filepath.Join(root, "manifests", "client")
			if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory_symlink":
				if err := os.Remove(filepath.Dir(filename)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Dir(filename)); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(first, filename); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				file, err := os.Create(filename)
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate((32 << 20) + 1)
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("create oversized document: %v, %v", err, closeErr)
				}
			}
			request.Metadata = json.RawMessage(`{"retention":{"keep":1},"pkginfo":{}}`)
			request.Method = "plan"
			_, err := munkirepo.Handle(t.Context(), request)
			want := "symlink"
			if kind == "oversized" {
				want = "retention read limit"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("unsafe reference document: %v", err)
			}
			if _, err := os.Stat(first); err != nil {
				t.Fatalf("old publication removed: %v", err)
			}
		})
	}
}
