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
	if remove["source_item"] != "Example.app" || remove["destination_path"] != "/Applications" || len(remove) != 2 {
		t.Fatalf("uninstall path: %#v", remove)
	}
	install := result["installs"].([]any)[0].(map[string]any)
	if install["CFBundleIdentifier"] != "example.app" {
		t.Fatalf("native key missing: %#v", install)
	}
}

func TestRemovalPreservesRenamedCopyDestination(t *testing.T) {
	result, err := munki.Render(munki.Input{Name: "Example", Version: "1", InstallerType: "copy_from_dmg", InstallerLocation: "Example.dmg", SHA256: strings.Repeat("b", 64), Size: 100, Metadata: json.RawMessage(`{"items_to_copy":[{"source_item":"Payload/Original.app","destination_path":"/Applications","destination_item":"Renamed.app","user":"root","mode":"o-w"}],"uninstall_method":"remove_copied_items"}`)})
	if err != nil {
		t.Fatal(err)
	}
	remove := result["items_to_remove"].([]map[string]string)[0]
	if len(remove) != 3 || remove["source_item"] != "Payload/Original.app" || remove["destination_path"] != "/Applications" || remove["destination_item"] != "Renamed.app" {
		t.Fatalf("native removal lost copy destination or retained permission settings: %#v", remove)
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

func TestNoPkgPreservesUpdateRelationshipWithoutInstaller(t *testing.T) {
	for _, value := range []string{`["Creative Suite"]`, `[]`} {
		input := munki.Input{Name: "Browser Authentication", Version: "1.0", InstallerType: "nopkg", Metadata: json.RawMessage(`{"update_for":` + value + `,"installcheck_script":"#!/bin/sh\nexit 1","postinstall_script":"#!/bin/sh\nexit 0"}`)}
		encoded, err := munki.Build(input)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if _, err := plist.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(result["update_for"])
		if err != nil || string(got) != value || result["installer_type"] != "nopkg" {
			t.Fatalf("native policy: %s, %v, %#v", got, err, result)
		}
		for _, key := range []string{"installer_item_hash", "installer_item_size", "installer_item_location", "receipts"} {
			if _, exists := result[key]; exists {
				t.Fatalf("nopkg has installer field %s", key)
			}
		}
	}
	if _, err := munki.DecodeMetadata(json.RawMessage(`{"update_for":null}`)); err == nil {
		t.Fatal("accepted null relationship list")
	}
}
