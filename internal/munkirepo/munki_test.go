package munkirepo_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/internal/munkirepo"
	"github.com/woodleighschool/stemma/plugin"
)

func TestOmittedNativeMetadataSurvivesReconciliation(t *testing.T) {
	for _, test := range []struct {
		name, filename, metadata string
		preserved                []string
	}{
		{"copied-app", "App.dmg", `{"installer_type":"copy_from_dmg","uninstallable":true,"uninstall_method":"remove_copied_items","items_to_copy":[{"source_item":"App.app","destination_path":"/Applications"}]}`, []string{"installer_type", "items_to_copy", "items_to_remove", "uninstall_method"}},
		{"package", "App.pkg", `{"uninstallable":true,"uninstall_method":"removepackages","receipts":[{"packageid":"example.app","version":"1"}]}`, []string{"receipts", "uninstall_method"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, request := repositoryRequest(t, test.filename, test.metadata)
			pkginfo := apply(t, root, request)
			old := readNative[map[string]any](t, pkginfo)
			old["vendor_extension"] = "keep"
			old["_metadata"].(map[string]any)["created_by"] = "operator"
			if test.name == "copied-app" {
				old["items_to_remove"] = []any{map[string]any{"path": "/Applications/App.app"}, map[string]any{"path": "/Library/Application Support/App"}}
			}
			writeNative(t, pkginfo, old)

			request.Metadata = json.RawMessage(`{"uninstallable":true,"description":"Updated description"}`)
			apply(t, root, request)
			got := readNative[map[string]any](t, pkginfo)
			for _, key := range test.preserved {
				if !reflect.DeepEqual(got[key], old[key]) {
					t.Errorf("omitted %s changed: got %#v, want %#v", key, got[key], old[key])
				}
			}
			if got["description"] != "Updated description" || got["vendor_extension"] != "keep" || got["_metadata"].(map[string]any)["created_by"] != "operator" {
				t.Fatalf("managed change or unowned native metadata was lost: %#v", got)
			}
			assertConverged(t, request)
			if test.name == "package" {
				request.Metadata = json.RawMessage(`{"receipts":[]}`)
				if _, err := munkirepo.Handle(t.Context(), request); err == nil {
					t.Fatal("cleared receipts required by the preserved removal method")
				}
				if after := readNative[map[string]any](t, pkginfo); !reflect.DeepEqual(after, got) {
					t.Fatal("invalid effective metadata changed the repository")
				}
			}
		})
	}
}

func TestCatalogsPreserveForeignNameAndVersionVariants(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{"name":"App","description":"First"}`)
	foreign := []map[string]any{
		{"name": "App", "version": "1", "supported_architectures": []string{"arm64"}, "installer_item_location": "foreign/arm.pkg"},
		{"name": "App", "version": "1", "installer_item_location": "foreign/intel.pkg", "_metadata": map[string]any{"stemma": "another-owner"}},
	}
	for _, name := range []string{"all", "testing"} {
		writeNative(t, filepath.Join(root, "catalogs", name), foreign)
	}
	apply(t, root, request)
	request.Metadata = json.RawMessage(`{"name":"App","description":"Second"}`)
	apply(t, root, request)
	for _, name := range []string{"all", "testing"} {
		entries := readNative[[]map[string]any](t, filepath.Join(root, "catalogs", name))
		if len(entries) != 3 {
			t.Fatalf("%s contains %d entries, want two foreign variants and one owned entry", name, len(entries))
		}
		locations := map[string]bool{}
		owned := 0
		for _, entry := range entries {
			location, _ := entry["installer_item_location"].(string)
			locations[location] = true
			if entry["description"] == "Second" {
				owned++
			}
		}
		if !locations["foreign/arm.pkg"] || !locations["foreign/intel.pkg"] || owned != 1 {
			t.Fatalf("%s replaced foreign variants or duplicated the owned entry: %#v", name, entries)
		}
	}
	assertConverged(t, request)
}

func TestRejectForeignPkginfoAtOwnedPath(t *testing.T) {
	for _, metadata := range []map[string]any{nil, {"stemma": "another-owner"}} {
		root, request := repositoryRequest(t, "App.pkg", `{}`)
		request.Method = "plan"
		response, err := munkirepo.Handle(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		pkginfo := bindingPath(t, root, response)
		foreign := map[string]any{"name": "Foreign", "version": "1"}
		if metadata != nil {
			foreign["_metadata"] = metadata
		}
		writeNative(t, pkginfo, foreign)
		before, err := os.ReadFile(pkginfo)
		if err != nil {
			t.Fatal(err)
		}
		request.Method = "apply"
		if _, err := munkirepo.Handle(t.Context(), request); err == nil {
			t.Fatal("replaced foreign pkginfo at the deterministic owned path")
		}
		after, err := os.ReadFile(pkginfo)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("failed ownership check changed existing pkginfo")
		}
	}
}

func TestInterruptedCatalogMembershipChangeConvergesOnRetry(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{"catalogs":["testing"]}`)
	pkginfo := apply(t, root, request)
	old := readNative[map[string]any](t, pkginfo)
	// Model an interrupted publication with the new catalog already present while
	// the old pkginfo still records every catalog that needs reconciliation.
	partial := readNative[map[string]any](t, pkginfo)
	partial["catalogs"] = []string{"production"}
	catalogDir := filepath.Join(root, "catalogs")
	writeNative(t, filepath.Join(catalogDir, "production"), []map[string]any{partial})
	if err := os.Chmod(catalogDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(catalogDir, 0o755) })
	probe, err := os.CreateTemp(catalogDir, "permission-probe-*")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("host does not enforce directory write permissions")
	}
	request.Metadata = json.RawMessage(`{"catalogs":["production"]}`)
	if _, err := munkirepo.Handle(t.Context(), request); err == nil {
		t.Fatal("publication succeeded with unwritable catalogs")
	}
	if got := readNative[map[string]any](t, pkginfo); !reflect.DeepEqual(got, old) {
		t.Fatal("failed catalog publication replaced the previous membership history")
	}
	if err := os.Chmod(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	apply(t, root, request)
	if entries := readNative[[]map[string]any](t, filepath.Join(catalogDir, "testing")); len(entries) != 0 {
		t.Fatalf("retry retained stale testing membership: %#v", entries)
	}
	for _, name := range []string{"all", "production"} {
		if entries := readNative[[]map[string]any](t, filepath.Join(catalogDir, name)); len(entries) != 1 {
			t.Fatalf("retry duplicated or lost %s entry: %#v", name, entries)
		}
	}
	assertConverged(t, request)
}

func repositoryRequest(t *testing.T, filename, metadata string) (string, plugin.Request) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	content := []byte("installer content")
	artifactPath := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(artifactPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	connection, err := json.Marshal(map[string]string{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	return root, plugin.Request{
		Method: "apply", Identity: plugin.Identity{Project: "test", Recipe: "App", Destination: "munki"},
		Config: connection, Metadata: json.RawMessage(metadata),
		Artifact: plugin.Artifact{Path: artifactPath, Filename: filename, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), Version: "1"},
	}
}

func apply(t *testing.T, root string, request plugin.Request) string {
	t.Helper()
	request.Method = "apply"
	response, err := munkirepo.Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return bindingPath(t, root, response)
}

func bindingPath(t *testing.T, root string, response plugin.Response) string {
	t.Helper()
	var binding struct {
		Pkginfo string `json:"pkginfo"`
	}
	if err := json.Unmarshal(response.Binding, &binding); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, filepath.FromSlash(binding.Pkginfo))
}

func assertConverged(t *testing.T, request plugin.Request) {
	t.Helper()
	request.Method = "plan"
	response, err := munkirepo.Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Changes) != 0 {
		t.Fatalf("unchanged reconciliation has drift: %#v", response.Changes)
	}
}

func readNative[T any](t *testing.T, filename string) T {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := munki.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func writeNative(t *testing.T, filename string, value any) {
	t.Helper()
	data, err := munki.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
