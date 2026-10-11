package intune

import (
	"reflect"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestIncludedSubjectsReachPreviewAndGraph(t *testing.T) {
	app := plugin.Subject{ID: "Payload/Example.app", Path: "Payload/Example.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "1.4", Build: "140"}}
	receipt := plugin.Subject{ID: "PackageInfo", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.installer", Version: "1.4.0", HasPayload: true}}
	other := plugin.Subject{ID: "Payload/Other.app", Kind: "app", InstalledPath: "/Applications/Other.app", App: &plugin.AppFacts{BundleID: "org.example.other", Version: "9.0"}}
	for _, test := range []struct {
		name, appType string
		subject       plugin.Subject
		id, version   string
	}{
		{"PKG application", "pkg", app, "org.example.app", "1.4"},
		{"DMG application", "dmg", app, "org.example.app", "1.4"},
		{"LOB application", "lob", app, "org.example.app", "1.4"},
		{"PKG receipt", "pkg", receipt, "org.example.installer", "1.4.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			native := object{"id": "org.example.declared", "version": "2.0"}
			req := lobRequest(object{"type": test.appType, "included_apps": []any{test.subject, native}}, &other, receipt, app, other)
			req.Artifact.SHA256 = strings.Repeat("a", 64)
			if test.appType == "dmg" {
				req.Artifact.Path, req.Artifact.Filename, req.Artifact.Format = "/leased/Example.dmg", "Example.dmg", "dmg"
			}
			if err := plugin.ValidateSchema(raw(MetadataSchema()), req.Metadata); err != nil {
				t.Fatalf("subject schema: %v", err)
			}
			response, err := Handle(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := decodeObject(response.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			included := object{"id": test.id, "version": test.version}
			if test.appType == "lob" {
				included["build"] = "140"
			}
			if !reflect.DeepEqual(metadata["included_apps"], []any{included, native}) || metadata["primary_bundle_id"] != test.id || metadata["primary_bundle_version"] != test.version {
				t.Fatalf("resolved detection: %s", response.Metadata)
			}
			compiled, err := compile(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if test.appType == "lob" {
				want := []any{object{"bundleId": test.id, "buildNumber": "1.4", "versionNumber": "140"}, object{"bundleId": "org.example.declared", "buildNumber": "2.0", "versionNumber": "2.0"}}
				if !reflect.DeepEqual(compiled["childApps"], want) || compiled["bundleId"] != test.id || compiled["buildNumber"] != "1.4" || compiled["versionNumber"] != "140" || metadata["primary_bundle_build"] != "140" {
					t.Fatalf("LOB detection: %s", raw(compiled))
				}
			} else {
				want := []any{object{"bundleId": test.id, "bundleVersion": test.version}, object{"bundleId": "org.example.declared", "bundleVersion": "2.0"}}
				if !reflect.DeepEqual(compiled["includedApps"], want) || compiled["primaryBundleId"] != test.id || compiled["primaryBundleVersion"] != test.version {
					t.Fatalf("Graph detection: %s", raw(compiled))
				}
			}
		})
	}
}

func TestIncludedSubjectsUseTheAppTypesDetectionRules(t *testing.T) {
	app := plugin.Subject{ID: "Example.app", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "1.4"}}
	outside := app
	outside.InstalledPath = "/Library/Example.app"
	noPath := app
	noPath.InstalledPath = ""
	receipt := plugin.Subject{ID: "PackageInfo", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.installer", Version: "1.4.0", HasPayload: true}}
	for _, test := range []struct {
		name, appType string
		items         []any
		failure       string
	}{
		{"PKG application outside Applications", "pkg", []any{outside}, ""},
		{"DMG application outside Applications", "dmg", []any{outside}, "does not install under /Applications"},
		{"LOB application without installed path", "lob", []any{noPath}, "does not install under /Applications"},
		{"DMG receipt", "dmg", []any{receipt}, "package subject requires a PKG app"},
		{"LOB receipt", "lob", []any{receipt}, "package subject requires a PKG app"},
		{"payloadless receipt", "pkg", []any{plugin.Subject{Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.script", Version: "1.0"}}}, "has no payload"},
		{"versionless receipt", "pkg", []any{plugin.Subject{Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.script", HasPayload: true}}}, "no selected application or package receipt"},
		{"missing bundle identifier", "pkg", []any{plugin.Subject{Kind: "app", App: &plugin.AppFacts{Version: "1.0"}}}, "no CFBundleIdentifier"},
		{"build without short version", "pkg", []any{plugin.Subject{Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.app", Build: "140"}}}, "no CFBundleShortVersionString"},
		{"unrelated subject", "pkg", []any{plugin.Subject{Kind: "msi", MSI: &plugin.MSIFacts{ProductName: "Example"}}}, "expected an identifier and version pair or an inspected application or package"},
		{"missing application facts", "pkg", []any{plugin.Subject{Kind: "app"}}, "expected an identifier and version pair or an inspected application or package"},
		{"malformed subject", "pkg", []any{object{"kind": "app", "app": "Example"}}, "invalid inspected subject"},
		{"unknown field", "pkg", []any{object{"kind": "app", "app": app.App, "version": "1.4"}}, "invalid inspected subject"},
		{"unknown application field", "pkg", []any{object{"kind": "app", "app": object{"bundleId": "org.example.app", "version": "1.4"}}}, "invalid inspected subject"},
		{"duplicate subjects", "pkg", []any{app, app}, "duplicate identifiers"},
		{"duplicate subject and native entry", "pkg", []any{app, object{"id": "org.example.app", "version": "2.0"}}, "duplicate identifiers"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := plugin.ReconcileRequest[Config]{Method: "validate", Metadata: raw(object{"type": test.appType, "included_apps": test.items})}
			_, err := Handle(t.Context(), req)
			if test.failure == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Fatalf("got %v, want %s", err, test.failure)
			}
		})
	}
}
