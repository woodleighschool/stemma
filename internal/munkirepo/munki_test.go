package munkirepo_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
			pkginfo := apply(t, root, &request)
			old := readNative[map[string]any](t, pkginfo)
			old["vendor_extension"] = "keep"
			old["_metadata"].(map[string]any)["created_by"] = "operator"
			if test.name == "copied-app" {
				old["items_to_remove"] = []any{map[string]any{"path": "/Applications/App.app"}, map[string]any{"path": "/Library/Application Support/App"}}
			}
			writeNative(t, pkginfo, old)

			request.Metadata = nativeMetadata(`{"uninstallable":true,"description":"Updated description"}`)
			apply(t, root, &request)
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
				request.Metadata = nativeMetadata(`{"receipts":[]}`)
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

func TestObservedPackageFormatDoesNotRequireFilenameExtension(t *testing.T) {
	root, request := repositoryRequest(t, "vendor-download", `{}`)
	request.Artifact.Format = "pkg"
	pkginfo := apply(t, root, &request)
	document := readNative[map[string]any](t, pkginfo)
	if document["installer_item_hash"] != request.Artifact.SHA256 {
		t.Fatal("the observed package did not retain its installer identity")
	}
	if _, exists := document["installer_type"]; exists {
		t.Fatal("native PKG publication should omit installer_type")
	}
	assertConverged(t, request)
}

func TestNoPkgDocumentWithoutInstaller(t *testing.T) {
	root, request := repositoryRequest(t, "pkginfo.json", `{}`)
	data := []byte(`{"name":"Browser Policy","version":"1.0","installer_type":"nopkg","update_for":["Creative Suite"],"installcheck_script":"#!/bin/sh\nexit 1","postinstall_script":"#!/bin/sh\nexit 0"}`)
	if err := os.WriteFile(request.Artifact.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	request.Artifact.SHA256, request.Artifact.Size, request.Artifact.Format = hex.EncodeToString(digest[:]), int64(len(data)), "json"
	request.Method = "validate"
	if _, err := munkirepo.Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	document := readNative[map[string]any](t, apply(t, root, &request))
	if document["installer_type"] != "nopkg" || document["version"] != "1.0" || document["name"] != "Browser Policy" {
		t.Fatalf("native policy changed: %#v", document)
	}
	if _, err := os.Stat(filepath.Join(root, "pkgs")); !os.IsNotExist(err) {
		t.Fatalf("nopkg created installer storage: %v", err)
	}
	assertConverged(t, request)

	request.Inputs = map[string]plugin.Artifact{"installer": request.Artifact}
	if _, err := munkirepo.Handle(t.Context(), request); err == nil {
		t.Fatal("nopkg accepted an installer input")
	}
	request.Inputs = nil
	request.Artifact.SHA256 = strings.Repeat("0", 64)
	if _, err := munkirepo.Handle(t.Context(), request); err == nil {
		t.Fatal("accepted tampered policy document")
	}
	for _, invalid := range []string{
		`{"name":"Policy","installer_type":"nopkg"}`,
		`{"name":"Policy","version":"1.0","installer_type":"nopkg","installer_item_size":0}`,
		`{"name":"App","version":"1.0","installer_type":"pkg"}`,
	} {
		if err := os.WriteFile(request.Artifact.Path, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(invalid))
		request.Artifact.SHA256, request.Artifact.Size = hex.EncodeToString(digest[:]), int64(len(invalid))
		if _, err := munkirepo.Handle(t.Context(), request); err == nil {
			t.Fatalf("accepted invalid installer-free document: %s", invalid)
		}
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
	apply(t, root, &request)
	request.Metadata = nativeMetadata(`{"name":"App","description":"Second"}`)
	apply(t, root, &request)
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
	pkginfo := apply(t, root, &request)
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
	request.Metadata = nativeMetadata(`{"catalogs":["production"]}`)
	if _, err := munkirepo.Handle(t.Context(), request); err == nil {
		t.Fatal("publication succeeded with unwritable catalogs")
	}
	if got := readNative[map[string]any](t, pkginfo); !reflect.DeepEqual(got, old) {
		t.Fatal("failed catalog publication replaced the previous membership history")
	}
	if err := os.Chmod(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	apply(t, root, &request)
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

func repositoryRequest(t *testing.T, filename, metadata string) (string, plugin.ReconcileRequest) {
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
	return root, plugin.ReconcileRequest{
		Method: "apply", Identity: plugin.Identity{Project: "test", Software: "App", Destination: "munki"},
		Config: connection, Metadata: nativeMetadata(metadata),
		Artifact: plugin.Artifact{Path: artifactPath, Filename: filename, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), Version: "1"},
	}
}

func apply(t *testing.T, root string, request *plugin.ReconcileRequest) string {
	t.Helper()
	request.Method = "apply"
	response, err := munkirepo.Handle(t.Context(), *request)
	if err != nil {
		t.Fatal(err)
	}
	request.Binding = response.Binding
	return bindingPath(t, root, response)
}

func bindingPath(t *testing.T, root string, response plugin.ReconcileResponse) string {
	t.Helper()
	var binding struct {
		Pkginfo string `json:"pkginfo"`
	}
	if err := json.Unmarshal(response.Binding, &binding); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, filepath.FromSlash(binding.Pkginfo))
}

func assertConverged(t *testing.T, request plugin.ReconcileRequest) {
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

func nativeMetadata(data string) json.RawMessage { return json.RawMessage(`{"pkginfo":` + data + `}`) }

func TestPreparedIconPublishesAndRetainsExplicitOverride(t *testing.T) {
	root, request := repositoryRequest(t, "Example.pkg", `{}`)
	data := []byte("synthetic PNG content")
	icon := filepath.Join(t.TempDir(), "icon.png")
	if err := os.WriteFile(icon, data, 0o400); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	artifact := plugin.Artifact{Path: icon, Filename: "icon.png", Format: "png", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
	request.Inputs = map[string]plugin.Artifact{"icon": artifact}
	pkginfo := apply(t, root, &request)
	values := readNative[map[string]any](t, pkginfo)
	name, _ := values["icon_name"].(string)
	published, err := os.ReadFile(filepath.Join(root, "icons", name))
	if err != nil || !bytes.Equal(data, published) || values["icon_hash"] != artifact.SHA256 {
		t.Fatalf("icon publication: %v %#v", err, values)
	}
	assertConverged(t, request)
	request.Inputs = nil
	apply(t, root, &request)
	values = readNative[map[string]any](t, pkginfo)
	if values["icon_name"] != name {
		t.Fatal("existing icon was removed")
	}
	request.Metadata = nativeMetadata(`{"icon_name":"manual.png","icon_hash":null}`)
	request.Inputs = map[string]plugin.Artifact{"icon": artifact}
	apply(t, root, &request)
	values = readNative[map[string]any](t, pkginfo)
	if values["icon_name"] != "manual.png" {
		t.Fatal("authored icon overwritten")
	}
}

func TestIconBootstrapRefreshAndRetention(t *testing.T) {
	root, request := repositoryRequest(t, "Example.pkg", `{}`)
	pkginfo := apply(t, root, &request)
	icon := func(value uint8) plugin.Artifact {
		t.Helper()
		img := image.NewRGBA(image.Rect(0, 0, 2, 2))
		img.SetRGBA(0, 0, color.RGBA{R: value, A: 255})
		var content bytes.Buffer
		if err := png.Encode(&content, img); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(t.TempDir(), "icon.png")
		if err := os.WriteFile(name, content.Bytes(), 0o400); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content.Bytes())
		return plugin.Artifact{Path: name, Filename: "icon.png", Format: "png", Size: int64(content.Len()), SHA256: hex.EncodeToString(digest[:])}
	}
	portable, native := icon(10), icon(200)
	request.Inputs = map[string]plugin.Artifact{"icon": portable}
	apply(t, root, &request)
	check := func(want plugin.Artifact) {
		t.Helper()
		values := readNative[map[string]any](t, pkginfo)
		if values["icon_hash"] != want.SHA256 {
			t.Fatalf("icon hash = %v, want %s", values["icon_hash"], want.SHA256)
		}
		if values["installer_item_hash"] != request.Artifact.SHA256 {
			t.Fatal("icon changed installer identity")
		}
	}
	check(portable)
	request.Inputs["icon"] = native
	apply(t, root, &request)
	check(portable)
	request.RefreshIcons = true
	request.Method = "plan"
	response, err := munkirepo.Handle(t.Context(), request)
	if err != nil || len(response.Changes) == 0 {
		t.Fatalf("refresh plan: %+v %v", response, err)
	}
	check(portable)
	apply(t, root, &request)
	check(native)
	request.RefreshIcons = false
	request.Inputs["icon"] = portable
	apply(t, root, &request)
	check(native)
	// A new software version retains the existing artwork too.
	request.Artifact.Version = "2.0"
	pkginfo = apply(t, root, &request)
	check(native)
	request.Artifact.Version = "3.0"
	request.Inputs = nil
	pkginfo = apply(t, root, &request)
	check(native)
	request.Inputs = map[string]plugin.Artifact{"icon": portable}
	// Remote absence is authoritative even when the software and binding exist.
	if err := os.Remove(filepath.Join(root, "icons", "stemma", native.SHA256+".png")); err != nil {
		t.Fatal(err)
	}
	apply(t, root, &request)
	check(portable)
	assertConverged(t, request)
}
