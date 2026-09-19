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

func TestRetentionPlansWithoutWritingAndCoversTheWholeFamily(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{}`)
	// The family is whatever the repository holds for the name, including
	// versions another tool imported, ordered as Munki orders versions.
	imported := map[string]string{}
	for _, version := range []string{"0.9", "0.10"} {
		imported[version] = filepath.Join(root, "pkgsinfo", "apps", "App-"+version+".plist")
		writeNative(t, imported[version], map[string]any{"name": "App", "version": version, "catalogs": []string{"testing"}, "installer_item_location": "apps/App-" + version + ".pkg"})
		if err := os.MkdirAll(filepath.Join(root, "pkgs", "apps"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "pkgs", "apps", "App-"+version+".pkg"), []byte(version), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeNative(t, filepath.Join(root, "manifests", "site_default"), map[string]any{"managed_installs": []string{"App"}})
	request.Metadata = json.RawMessage(`{"retention":{"keep":2},"pkginfo":{}}`)
	request.Method = "plan"
	response, err := munkirepo.Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var planned []string
	for _, change := range response.Changes {
		if change.Kind == "retention" {
			planned = append(planned, change.Field)
		}
	}
	if len(planned) != 1 || planned[0] != "pkgsinfo/apps/App-0.9.plist" {
		t.Fatalf("planned retention: %v", planned)
	}
	if _, err := os.Stat(imported["0.9"]); err != nil {
		t.Fatal("plan deleted a publication")
	}
	apply(t, root, &request)
	if _, err := os.Stat(imported["0.9"]); !os.IsNotExist(err) {
		t.Fatal("the oldest version outlived retention")
	}
	if _, err := os.Stat(filepath.Join(root, "pkgs", "apps", "App-0.9.pkg")); !os.IsNotExist(err) {
		t.Fatal("the pruned version kept its unreferenced installer")
	}
	if _, err := os.Stat(imported["0.10"]); err != nil {
		t.Fatal("a manifest naming the item held no version, yet the newer version was pruned")
	}
	assertConverged(t, request)
}

func TestRetentionPreservesBareReferencesWithDifferentAvailability(t *testing.T) {
	for _, test := range []struct {
		field  string
		before any
		after  any
	}{
		{"catalogs", []string{"production"}, []string{"testing"}},
		{"supported_architectures", []string{"arm64"}, nil},
		{"minimum_os_version", "12.0", "13.0"},
		{"maximum_os_version", "14.0", "15.0"},
		{"minimum_munki_version", "6.0", "7.0"},
		{"installable_condition", "machine_type == 'laptop'", "machine_type == 'desktop'"},
	} {
		t.Run(test.field, func(t *testing.T) {
			root, request := repositoryRequest(t, "App.pkg", `{}`)
			request.Metadata, _ = json.Marshal(map[string]any{"pkginfo": map[string]any{test.field: test.before}})
			first := apply(t, root, &request)
			catalog := "testing"
			if test.field == "catalogs" {
				catalog = "production"
			}
			writeNative(t, filepath.Join(root, "manifests", "client"), map[string]any{"catalogs": []string{catalog}, "managed_installs": []string{"App"}})
			metadata := map[string]any{}
			if test.after != nil {
				metadata[test.field] = test.after
			}
			request.Metadata, _ = json.Marshal(map[string]any{"retention": map[string]int{"keep": 1}, "pkginfo": metadata})
			request.Artifact.Version = "2"
			for _, method := range []string{"plan", "apply"} {
				request.Method = method
				response, err := munkirepo.Handle(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				for _, change := range response.Changes {
					if change.Kind == "retention" {
						t.Fatalf("%s prunes a referenced publication with different %s", method, test.field)
					}
				}
			}
			if _, err := os.Stat(first); err != nil {
				t.Fatalf("referenced publication removed: %v", err)
			}
		})
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

func TestInstallerFormatChangeClearsInapplicableDerivedFields(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{}`)
	pkg := plugin.Facts{Subjects: []plugin.Subject{
		{Kind: "package", Package: &plugin.PackageFacts{Identifier: "example.app", Version: "1", HasPayload: true, InstalledSize: 20}},
		{Kind: "installer", Installer: &plugin.InstallerFacts{RestartAction: "RequireRestart"}},
	}}
	request.Facts = pkg
	file := apply(t, root, &request)
	request.Artifact.Filename, request.Artifact.Format = "App.dmg", "dmg"
	request.Facts = plugin.Facts{Subjects: []plugin.Subject{{Kind: "app", Path: "App.app", App: &plugin.AppFacts{BundleID: "example.app", Version: "1"}}}}
	apply(t, root, &request)
	document := readNative[map[string]any](t, file)
	for _, field := range []string{"receipts", "installed_size", "RestartAction"} {
		if _, exists := document[field]; exists {
			t.Fatalf("DMG retained PKG-derived %s", field)
		}
	}
	if document["items_to_copy"] == nil || document["uninstall_method"] != "remove_copied_items" {
		t.Fatalf("DMG copy and removal metadata: %#v", document)
	}
	request.Artifact.Filename, request.Artifact.Format, request.Facts = "App.pkg", "pkg", pkg
	apply(t, root, &request)
	document = readNative[map[string]any](t, file)
	for _, field := range []string{"items_to_copy", "items_to_remove", "installs"} {
		if _, exists := document[field]; exists {
			t.Fatalf("PKG retained DMG-derived %s", field)
		}
	}
	if document["receipts"] == nil || document["uninstall_method"] != "removepackages" {
		t.Fatalf("PKG receipt and removal metadata: %#v", document)
	}
	assertConverged(t, request)
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
				want = "read limit"
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
