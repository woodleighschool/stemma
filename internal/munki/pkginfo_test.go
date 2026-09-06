package munki_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/munki"
	"howett.net/plist"
)

func TestPkginfoKeepsNativeSemanticsAndExactContent(t *testing.T) {
	encoded, err := munki.Build(munki.Input{Name: "Example App", Version: "2.1", InstallerType: "copy_from_dmg", InstallerLocation: "Example.dmg", SHA256: strings.Repeat("a", 64), Size: 1025, Metadata: json.RawMessage(`{"description":null,"unattended_install":false,"blocking_applications":[],"items_to_copy":[{"source_item":"Example.app","destination_path":"/Applications"}],"uninstallable":true,"uninstall_method":"remove_copied_items","installs":[{"type":"application","path":"/Applications/Example.app","CFBundleIdentifier":"example.app","CFBundleVersion":"21"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if _, err := plist.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if result["installer_item_hash"] != strings.Repeat("a", 64) || result["installer_item_size"] != uint64(2) {
		t.Fatalf("content identity = %#v", result)
	}
	if result["unattended_install"] != false || len(result["blocking_applications"].([]any)) != 0 {
		t.Fatalf("explicit zero values lost: %#v", result)
	}
	if _, exists := result["description"]; exists {
		t.Fatal("cleared description emitted")
	}
	remove := result["items_to_remove"].([]any)[0].(map[string]any)
	if remove["path"] != "/Applications/Example.app" {
		t.Fatalf("uninstall path: %#v", remove)
	}
	install := result["installs"].([]any)[0].(map[string]any)
	if install["CFBundleIdentifier"] != "example.app" {
		t.Fatalf("native key missing: %#v", install)
	}
}

func TestMetadataRejectsUnsupportedOrAmbiguousValues(t *testing.T) {
	for _, input := range []string{`{"unknown":true}`, `{"unattended_install":null}`, `{"blocking_applications":null}`, `{"installed_size":-1}`, `{"installs":[{"type":"application","path":"/Applications/App.app","unknown":true}]}`, `{"items_to_copy":[{"source_item":"../escape.app","destination_path":"/Applications"}]}`} {
		t.Run(input, func(t *testing.T) {
			if _, err := munki.DecodeMetadata(json.RawMessage(input)); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
	metadata, err := munki.DecodeMetadata(json.RawMessage(`{"description":null,"unattended_install":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(metadata.Fields["description"]) != "null" || metadata.UnattendedInstall == nil || *metadata.UnattendedInstall {
		t.Fatalf("presence was collapsed: %+v", metadata)
	}
	if _, exists := metadata.Fields["category"]; exists {
		t.Fatal("absent category gained ownership")
	}
}

func TestVendorPackageIsNotReinterpretedAsCopyFromImage(t *testing.T) {
	input := munki.Input{Name: "Vendor Driver", Version: "6.0", InstallerType: "pkg", InstallerLocation: "Vendor.pkg", SHA256: strings.Repeat("b", 64), Size: 64, Metadata: json.RawMessage(`{"receipts":[{"packageid":"example.vendor.driver","version":"6.0"}],"uninstallable":true,"uninstall_method":"removepackages"}`)}
	encoded, err := munki.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if _, err := plist.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if _, exists := result["installer_type"]; exists {
		t.Fatal("native pkg must omit installer_type")
	}
	if _, exists := result["items_to_copy"]; exists {
		t.Fatal("vendor installer became a copy operation")
	}
	input.InstallerType = "nopkg"
	if _, err := munki.Build(input); err == nil {
		t.Fatal("nopkg accepted installer bytes")
	}
}
