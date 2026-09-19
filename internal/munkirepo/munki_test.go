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
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/internal/munkirepo"
	"github.com/woodleighschool/stemma/plugin"
)

func TestOmittedNativeMetadataSurvivesReconciliation(t *testing.T) {
	for _, test := range []struct {
		name, filename, first, second string
	}{
		{
			"copied-app", "App.dmg",
			`{"installer_type":"copy_from_dmg","display_name":"App","uninstallable":true,"uninstall_method":"remove_copied_items","items_to_copy":[{"source_item":"App.app","destination_path":"/Applications"}]}`,
			`{"uninstallable":true,"items_to_copy":[{"source_item":"App.app","destination_path":"/Applications"}],"description":"Updated description"}`,
		},
		{
			"package", "App.pkg",
			`{"display_name":"App","uninstallable":true,"uninstall_method":"removepackages","receipts":[{"packageid":"example.app","version":"1"}]}`,
			`{"uninstallable":true,"receipts":[{"packageid":"example.app","version":"1"}],"description":"Updated description"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, request := repositoryRequest(t, test.filename, test.first)
			pkginfo := apply(t, root, &request)
			old := readNative[map[string]any](t, pkginfo)
			old["display_name"] = "Operator title"
			old["vendor_extension"] = "keep"
			old["_metadata"] = map[string]any{"created_by": "operator"}
			writeNative(t, pkginfo, old)

			request.Metadata = nativeMetadata(test.second)
			apply(t, root, &request)
			got := readNative[map[string]any](t, pkginfo)
			if got["display_name"] != "Operator title" || got["vendor_extension"] != "keep" || got["_metadata"].(map[string]any)["created_by"] != "operator" {
				t.Fatalf("omitted native metadata changed: %#v", got)
			}
			// The removal method is derived again from the declared detection.
			if got["description"] != "Updated description" || !reflect.DeepEqual(got["uninstall_method"], old["uninstall_method"]) || !reflect.DeepEqual(got["items_to_remove"], old["items_to_remove"]) {
				t.Fatalf("declared or derived metadata was lost: %#v", got)
			}
			assertConverged(t, request)
			if test.name == "package" {
				request.Metadata = nativeMetadata(`{"uninstallable":true,"receipts":[]}`)
				if _, err := munkirepo.Handle(t.Context(), request); err == nil {
					t.Fatal("an uninstallable item without a removal method was accepted")
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
	apply(t, root, &request)
	document := readNative[map[string]any](t, published(t, root, "Browser Policy", "1.0"))
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

func TestCatalogsReplaceTheItemAndPreserveItsVariants(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{"name":"App","description":"First","notes":"operator only"}`)
	existing := []map[string]any{
		{"name": "App", "version": "1", "supported_architectures": []string{"arm64"}, "installer_item_location": "variant/arm.pkg"},
		{"name": "App", "version": "1", "description": "Stale", "installer_item_location": "stale.pkg"},
		{"name": "Other", "version": "1", "installer_item_location": "other.pkg"},
	}
	for _, name := range []string{"all", "testing"} {
		writeNative(t, filepath.Join(root, "catalogs", name), existing)
	}
	pkginfo := apply(t, root, &request)
	request.Metadata = nativeMetadata(`{"name":"App","description":"Second","notes":"operator only"}`)
	apply(t, root, &request)
	if readNative[map[string]any](t, pkginfo)["notes"] != "operator only" {
		t.Fatal("pkginfo lost its administrator notes")
	}
	for _, name := range []string{"all", "testing"} {
		entries := readNative[[]map[string]any](t, filepath.Join(root, "catalogs", name))
		if len(entries) != 3 {
			t.Fatalf("%s contains %d entries, want the variant, the other item and one current entry", name, len(entries))
		}
		locations := map[string]bool{}
		for _, entry := range entries {
			location, _ := entry["installer_item_location"].(string)
			locations[location] = true
			if _, exists := entry["notes"]; exists {
				t.Fatalf("%s published administrator notes: %#v", name, entry)
			}
			if entry["installer_item_location"] == "App.pkg" && entry["description"] != "Second" {
				t.Fatalf("%s kept a stale entry: %#v", name, entry)
			}
		}
		if !locations["variant/arm.pkg"] || !locations["other.pkg"] || !locations["App.pkg"] || locations["stale.pkg"] {
			t.Fatalf("%s lost a variant or kept the replaced entry: %#v", name, entries)
		}
	}
	assertConverged(t, request)
}

func TestNewPkginfoNeverReplacesAnotherItemsFile(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{}`)
	occupied := filepath.Join(root, "pkgsinfo", "App-1.plist")
	writeNative(t, occupied, map[string]any{"name": "Foreign", "version": "1"})
	before, err := os.ReadFile(occupied)
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, root, &request); got != filepath.Join(root, "pkgsinfo", "App-1__1.plist") {
		t.Fatalf("pkginfo published at %s", got)
	}
	if after, err := os.ReadFile(occupied); err != nil || !bytes.Equal(before, after) {
		t.Fatal("publication changed another item's pkginfo")
	}
	assertConverged(t, request)
}

func TestExistingRepositoryItemIsAdoptedInPlace(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{"description":"Managed"}`)
	installer, err := os.ReadFile(request.Artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkgs", "apps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkgs", "apps", "App-1.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	// Desktop tools leave dotfiles that Munki itself ignores.
	if err := os.WriteFile(filepath.Join(root, "pkgs", "apps", ".DS_Store"), []byte{0, 1}, 0o644); err != nil {
		t.Fatal(err)
	}
	imported := map[string]any{"name": "App", "version": "1", "catalogs": []string{"production"}, "display_name": "Operator title", "installer_item_location": "apps/App-1.pkg", "installer_item_hash": request.Artifact.SHA256, "installer_item_size": 1, "_metadata": map[string]any{"created_by": "operator"}}
	pkginfo := filepath.Join(root, "pkgsinfo", "apps", "App-1.plist")
	writeNative(t, pkginfo, imported)
	if err := os.WriteFile(filepath.Join(root, "pkgsinfo", "apps", ".DS_Store"), []byte{0, 1}, 0o644); err != nil {
		t.Fatal(err)
	}
	writeNative(t, filepath.Join(root, "catalogs", "production"), []map[string]any{imported})
	request.Method = "plan"
	response, err := munkirepo.Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range response.Changes {
		if change.Kind == "content" {
			t.Fatalf("matching installer bytes planned an upload: %+v", change)
		}
	}
	if got := apply(t, root, &request); got != pkginfo {
		t.Fatalf("existing item republished at %s", got)
	}
	document := readNative[map[string]any](t, pkginfo)
	if document["description"] != "Managed" || document["display_name"] != "Operator title" || document["installer_item_location"] != "apps/App-1.pkg" {
		t.Fatalf("adoption lost its repository layout or omitted metadata: %#v", document)
	}
	if entries := readNative[[]map[string]any](t, filepath.Join(root, "catalogs", "production")); len(entries) != 1 || entries[0]["description"] != "Managed" || entries[0]["_metadata"] != nil {
		t.Fatalf("catalog entry: %#v", entries)
	}
	assertConverged(t, request)

	// The same version with different bytes is drift, replaced where it lives.
	replacement := []byte("rebuilt installer content")
	if err := os.WriteFile(request.Artifact.Path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(replacement)
	request.Artifact.SHA256, request.Artifact.Size = hex.EncodeToString(digest[:]), int64(len(replacement))
	request.Method = "plan"
	response, err = munkirepo.Handle(t.Context(), request)
	if err != nil || len(response.Changes) == 0 || response.Changes[0].Kind != "content" {
		t.Fatalf("changed bytes plan: %+v %v", response.Changes, err)
	}
	apply(t, root, &request)
	if published, err := os.ReadFile(filepath.Join(root, "pkgs", "apps", "App-1.pkg")); err != nil || !bytes.Equal(published, replacement) {
		t.Fatalf("installer was not replaced in place: %v", err)
	}
	if readNative[map[string]any](t, pkginfo)["installer_item_hash"] != request.Artifact.SHA256 {
		t.Fatal("pkginfo kept the replaced installer hash")
	}
	assertConverged(t, request)

	writeNative(t, filepath.Join(root, "pkgsinfo", "Bundle-1.plist"), map[string]any{"name": "Bundle", "version": "1", "installer_item_location": "apps/App-1.pkg"})
	if err := os.WriteFile(request.Artifact.Path, installer, 0o600); err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(installer)
	request.Artifact.SHA256, request.Artifact.Size = hex.EncodeToString(digest[:]), int64(len(installer))
	if _, err := munkirepo.Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("replaced installer bytes another item installs: %v", err)
	}
}

func TestArchitectureVariantsAreSeparateItems(t *testing.T) {
	root, arm := repositoryRequest(t, "App-arm64.pkg", `{"supported_architectures":["arm64"]}`)
	intel := arm
	intel.Artifact.Filename = "App-x86_64.pkg"
	intel.Metadata = nativeMetadata(`{"supported_architectures":["x86_64"]}`)
	first, second := apply(t, root, &arm), apply(t, root, &intel)
	if first == second || filepath.Base(first) != "App-1-arm64.plist" || filepath.Base(second) != "App-1-x86_64.plist" {
		t.Fatalf("variants published at %s and %s", first, second)
	}
	assertConverged(t, arm)
	assertConverged(t, intel)
	// One variant's retention never reaches the other's publications.
	arm.Artifact.Version = "2"
	arm.Metadata = json.RawMessage(`{"retention":{"keep":1},"pkginfo":{"supported_architectures":["arm64"]}}`)
	apply(t, root, &arm)
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("retention kept the variant's own old version")
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal("retention removed another variant")
	}
	// Without a declared architecture the declaration cannot choose a variant.
	unscoped := intel
	unscoped.Metadata = nativeMetadata(`{}`)
	writeNative(t, filepath.Join(root, "pkgsinfo", "App-1-arm64.plist"), map[string]any{"name": "App", "version": "1", "supported_architectures": []string{"arm64"}})
	if _, err := munkirepo.Handle(t.Context(), unscoped); err == nil || !strings.Contains(err.Error(), "supported_architectures") {
		t.Fatalf("ambiguous variants: %v", err)
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
		Method: "apply", Identity: plugin.Identity{Project: "test", Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "App"}, Destination: "munki"},
		Config: connection, Metadata: nativeMetadata(metadata),
		Artifact: plugin.Artifact{Path: artifactPath, Filename: filename, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), Version: "1"},
	}
}

func apply(t *testing.T, root string, request *plugin.ReconcileRequest) string {
	t.Helper()
	request.Method = "apply"
	if _, err := munkirepo.Handle(t.Context(), *request); err != nil {
		t.Fatal(err)
	}
	var declared struct {
		Pkginfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"pkginfo"`
	}
	if err := json.Unmarshal(request.Metadata, &declared); err != nil {
		t.Fatal(err)
	}
	name, version := request.Identity.Resource.Name, request.Artifact.Version
	if declared.Pkginfo.Name != "" {
		name = declared.Pkginfo.Name
	}
	if declared.Pkginfo.Version != "" {
		version = declared.Pkginfo.Version
	}
	if request.Artifact.Format == "json" {
		return ""
	}
	return published(t, root, name, version)
}

// published finds the newest-written pkginfo for an item the way the
// repository identifies it, by Munki name and version.
func published(t *testing.T, root, name, version string) string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(filepath.Join(root, "pkgsinfo"), func(filename string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			return err
		}
		document := readNative[map[string]any](t, filename)
		if document["name"] == name && document["version"] == version {
			found = append(found, filename)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatalf("no pkginfo publishes %s %s", name, version)
	}
	slices.Sort(found)
	return found[len(found)-1]
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
		t.Fatal("declared icon overwritten")
	}
}

func TestDeclaredIconReplacesOnChangeAndStaysWhenUndeclared(t *testing.T) {
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
	first, second := icon(10), icon(200)
	request.Inputs = map[string]plugin.Artifact{"icon": first}
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
	check(first)
	// Changed bytes are ordinary drift: planning reports them, applying replaces them.
	request.Inputs["icon"] = second
	request.Method = "plan"
	response, err := munkirepo.Handle(t.Context(), request)
	if err != nil || len(response.Changes) == 0 {
		t.Fatalf("changed icon plan: %+v %v", response, err)
	}
	check(first)
	apply(t, root, &request)
	check(second)
	assertConverged(t, request)
	// A new software version publishes the declared artwork again.
	request.Artifact.Version = "2.0"
	pkginfo = apply(t, root, &request)
	check(second)
	// Without a declared icon the artwork reference is left unchanged.
	request.Artifact.Version = "3.0"
	request.Inputs = nil
	pkginfo = apply(t, root, &request)
	check(second)
	// Repository absence is authoritative even when the pkginfo references it.
	request.Inputs = map[string]plugin.Artifact{"icon": second}
	content := filepath.Join(root, "icons", "stemma", second.SHA256+".png")
	if err := os.Remove(content); err != nil {
		t.Fatal(err)
	}
	apply(t, root, &request)
	check(second)
	if _, err := os.Stat(content); err != nil {
		t.Fatalf("missing icon content was not republished: %v", err)
	}
	assertConverged(t, request)
}
