package intune

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/intunewin"
	"github.com/woodleighschool/stemma/plugin"
)

func TestValidationPreviewsManagedWindowsMetadata(t *testing.T) {
	msi := &plugin.MSIFacts{ProductCode: "{11111111-1111-4111-8111-111111111111}", ProductVersion: "2.0", ProductName: "Example", Manufacturer: "Installer Vendor"}
	req := plugin.ReconcileRequest[Config]{
		Method: "validate", Prepared: true,
		Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: "example"}},
		Artifact: plugin.Artifact{Path: "/leased/Example.msi", Filename: "Example.msi", SHA256: strings.Repeat("a", 64), Version: "2.0.1", Evidence: selectedMSI(msi)},
		Metadata: raw(object{
			"display_name": "Declared name", "architectures": nil, "categories": []any{}, "dependencies": []any{},
			"msi":                object{"publisher": "Declared vendor"},
			"msi_properties":     object{"PORTAL": "vpn.example.test"},
			"install_experience": object{"run_as": "system", "restart": "based_on_return_code"},
			"assignments":        []any{object{"intent": "available_without_enrollment", "all_users": true, "filter": nil, "notifications": "hide_all"}},
			"detection":          []any{object{"type": "file", "path": `C:\Program Files\Example`, "name": "Example.exe", "property": "version"}},
		}),
	}
	response, err := Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := decodeObject(response.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	want := object{
		"type": "win32", "display_name": "Declared name", "architectures": nil, "categories": []any{}, "dependencies": []any{},
		"setup_file":         "Example.msi",
		"msi":                object{"product_code": msi.ProductCode, "product_version": "2.0", "product_name": "Example", "publisher": "Declared vendor", "upgrade_code": nil},
		"install_command":    `msiexec /i "Example.msi" /qn /norestart PORTAL="vpn.example.test"`,
		"uninstall_command":  `msiexec /x "` + msi.ProductCode + `" /qn /norestart`,
		"install_experience": object{"run_as": "system", "restart": "based_on_return_code"},
		"assignments":        []any{object{"intent": "available_without_enrollment", "all_users": true, "filter": nil, "notifications": "hide_all"}},
		"detection":          []any{object{"type": "file", "path": `C:\Program Files\Example`, "name": "Example.exe", "property": "version", "operator": "greater_than_or_equal", "value": "2.0.1"}},
	}
	if !reflect.DeepEqual(metadata, want) {
		t.Fatalf("preview = %s, want %s", response.Metadata, raw(want))
	}
	if len(response.Changes) != 0 || response.Origins["msi.upgrade_code"] != "windows.installer" || response.Origins["msi.publisher"] != "" || response.Origins["detection.0.value"] != "artifact.version" {
		t.Fatalf("preview changes or origins: %+v", response)
	}
	compiled, err := compile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	wantGraph := object{
		"@odata.type": win32Type, "displayName": "Declared name", "allowedArchitectures": nil, "categories": []any{}, "dependencies": []any{},
		"setupFilePath":        "Example.msi",
		"msiInformation":       object{"productCode": msi.ProductCode, "productVersion": "2.0", "productName": "Example", "publisher": "Declared vendor", "upgradeCode": nil},
		"installCommandLine":   want["install_command"],
		"uninstallCommandLine": want["uninstall_command"],
		"installExperience":    object{"runAsAccount": "system", "deviceRestartBehavior": "basedOnReturnCode"},
		"assignments": []any{object{
			"intent": "availableWithoutEnrollment",
			"target": object{
				"@odata.type": "#microsoft.graph.allLicensedUsersAssignmentTarget", "deviceAndAppManagementAssignmentFilterId": nil, "deviceAndAppManagementAssignmentFilterType": "none",
			},
			"settings": object{"@odata.type": "#microsoft.graph.win32LobAppAssignmentSettings", "notifications": "hideAll"},
		}},
		"rules": []any{object{"@odata.type": "#microsoft.graph.win32LobAppFileSystemRule", "ruleType": "detection", "path": `C:\Program Files\Example`, "fileOrFolderName": "Example.exe", "operationType": "version", "operator": "greaterThanOrEqual", "comparisonValue": "2.0.1"}},
	}
	if !reflect.DeepEqual(compiled, wantGraph) {
		t.Fatalf("Graph metadata = %s, want %s", raw(compiled), raw(wantGraph))
	}

	req.Prepared, req.Artifact = false, plugin.Artifact{}
	response, err = Handle(t.Context(), req)
	if err != nil {
		t.Fatalf("validation without preparation: %v", err)
	}
	metadata, err = decodeObject(response.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := metadata["install_command"]; exists {
		t.Fatal("validation without an artifact invented an install command")
	}
	if _, exists := metadata["setup_file"]; exists {
		t.Fatal("validation without an artifact invented a setup file")
	}
	if _, exists := metadata["detection"].([]any)[0].(object)["value"]; exists {
		t.Fatal("validation without an artifact invented a detected version")
	}
}

func TestValidationPreviewsThePreparedSetupEntrypoint(t *testing.T) {
	tree := treeArtifact(t)
	envelope := filepath.Join(t.TempDir(), "Example.intunewin")
	if _, err := intunewin.Write(t.Context(), tree.Path, tree.EntryPoint, envelope); err != nil {
		t.Fatal(err)
	}
	for name, artifact := range map[string]plugin.Artifact{
		"setup tree":        tree,
		"existing envelope": {Path: envelope, Filename: "Example.intunewin", SHA256: strings.Repeat("a", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			req := plugin.ReconcileRequest[Config]{Method: "validate", Prepared: true, Metadata: raw(object{"type": "win32"}), Artifact: artifact}
			response, err := Handle(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := decodeObject(response.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			if metadata["setup_file"] != `bin\setup.cmd` || response.Origins["setup_file"] != "installer.entry_point" {
				t.Fatalf("prepared setup file = %v from %q", metadata["setup_file"], response.Origins["setup_file"])
			}
		})
	}
}

func TestValidationPreviewsMacDetectionAndMinimumOS(t *testing.T) {
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container"}
	receipt := plugin.Subject{ID: "PackageInfo", Parent: ".", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.installer", Version: "1.4", HasPayload: true}}
	app := plugin.Subject{ID: "Payload/Example.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "1.4", Build: "140"}}
	for _, appType := range []string{"pkg", "dmg", "lob"} {
		t.Run(appType, func(t *testing.T) {
			req := lobRequest(object{"type": appType, "ignore_version_detection": false}, &app, root, receipt, app)
			req.Artifact.SHA256 = strings.Repeat("a", 64)
			req.MinimumOS = &plugin.MinimumOS{Version: "14.2", Origin: "app.minimum_os"}
			if appType == "dmg" {
				req.Artifact.Path, req.Artifact.Filename, req.Artifact.Format = "/leased/Example.dmg", "Example.dmg", "dmg"
			}
			if appType == "pkg" {
				req.Metadata = raw(object{"type": appType, "ignore_version_detection": false, "post_install_script": nil})
			}
			response, err := Handle(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := decodeObject(response.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			included := object{"id": "org.example.app", "version": "1.4"}
			want := object{"type": appType, "ignore_version_detection": false, "minimum_os": "14.0", "included_apps": []any{included}, "primary_bundle_id": "org.example.app", "primary_bundle_version": "1.4"}
			if appType == "lob" {
				included["build"], want["primary_bundle_build"] = "140", "140"
			}
			if appType == "pkg" {
				want["post_install_script"] = nil
			}
			if !reflect.DeepEqual(metadata, want) || response.Origins["minimum_os"] != "app.minimum_os 14.2 → 14.0" {
				t.Fatalf("preview = %s from %+v, want %s", response.Metadata, response.Origins, raw(want))
			}
			compiled, err := compile(metadata)
			if err != nil {
				t.Fatal(err)
			}
			wantGraph := object{"@odata.type": appTypes[appType], "ignoreVersionDetection": false, "minimumSupportedOperatingSystem": object{"v14_0": true}}
			if appType == "lob" {
				wantGraph["childApps"] = []any{object{"bundleId": "org.example.app", "buildNumber": "1.4", "versionNumber": "140"}}
				wantGraph["bundleId"], wantGraph["buildNumber"], wantGraph["versionNumber"] = "org.example.app", "1.4", "140"
			} else {
				wantGraph["includedApps"] = []any{object{"bundleId": "org.example.app", "bundleVersion": "1.4"}}
				wantGraph["primaryBundleId"], wantGraph["primaryBundleVersion"] = "org.example.app", "1.4"
			}
			if appType == "pkg" {
				wantGraph["postInstallScript"] = nil
			}
			if !reflect.DeepEqual(compiled, wantGraph) {
				t.Fatalf("Graph metadata = %s, want %s", raw(compiled), raw(wantGraph))
			}
		})
	}
}
