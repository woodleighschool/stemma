package munki_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/plugin"
)

func TestInstallsTranslatesInspectedApplications(t *testing.T) {
	subject := func(s plugin.Subject) string {
		data, _ := json.Marshal(s)
		return string(data)
	}
	packaged := subject(plugin.Subject{ID: "Suite.pkg/Payload/Draw.app", Kind: "app", Path: "Suite.pkg/Payload/Draw.app", InstalledPath: "/Applications/Draw.app", App: &plugin.AppFacts{BundleID: "org.example.draw", Name: "Draw", Version: "26.1", Build: "2610", MinimumOS: "13.0"}})
	imaged := subject(plugin.Subject{ID: "Tools/Graph.app", Kind: "app", Path: "Tools/Graph.app", App: &plugin.AppFacts{BundleID: "org.example.graph", Version: "beta", Build: "83"}})
	native := `{"type":"file","path":"/Library/Suite/licence"}`
	destination, err := munki.DecodeDestination(json.RawMessage(`{"pkginfo":{"items_to_copy":[{"source_item":"Tools","destination_path":"/Applications","destination_item":"Suite"}],"installs":[` + packaged + `,` + native + `,` + imaged + `]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Installs []munki.InstallItem `json:"installs"`
	}
	if err := json.Unmarshal(destination.Pkginfo, &metadata); err != nil {
		t.Fatal(err)
	}
	installs := metadata.Installs
	if len(installs) != 3 || installs[0] != (munki.InstallItem{Type: "application", Path: "/Applications/Draw.app", BundleIdentifier: "org.example.draw", BundleName: "Draw", BundleShortVersion: "26.1", BundleVersion: "2610", VersionComparisonKey: "CFBundleShortVersionString", MinimumOSVersion: "13.0"}) {
		t.Fatalf("installs = %+v", installs)
	}
	// A disk image's application is detected where items_to_copy puts it, by
	// its build when the short version does not compare.
	if installs[1].Type != "file" || installs[2].Path != "/Applications/Suite/Graph.app" || installs[2].VersionComparisonKey != "CFBundleVersion" {
		t.Fatalf("installs = %+v", installs)
	}
	for item, failure := range map[string]string{
		subject(plugin.Subject{ID: "Suite.pkg/PackageInfo", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.suite"}}):        "expected a native installs item or an inspected application",
		subject(plugin.Subject{ID: "Loose.app", Kind: "app", Path: "Loose.app", App: &plugin.AppFacts{BundleID: "org.example.loose", Version: "1"}}): "application Loose.app has no known installed path",
		subject(plugin.Subject{ID: "Bare.app", Kind: "app", InstalledPath: "/Applications/Bare.app", App: &plugin.AppFacts{Version: "1"}}):           "application Bare.app: bundle identifier and comparison version are required",
	} {
		_, err := munki.DecodeDestination(json.RawMessage(`{"pkginfo":{"installs":[` + item + `]}}`))
		if err == nil || !strings.Contains(err.Error(), "installs item 1: "+failure) {
			t.Errorf("%s: %v", item, err)
		}
	}
	for _, copies := range []string{
		`[{"source_item":"Suite.pkg/Payload/Draw.app","destination_path":"/Applications/Other"}]`,
		`[{"source_item":"Tools","destination_path":"/Applications"},{"source_item":"Tools/Graph.app","destination_path":"/Applications"}]`,
	} {
		_, err := munki.DecodeDestination(json.RawMessage(`{"pkginfo":{"items_to_copy":` + copies + `,"installs":[` + packaged + `,` + imaged + `]}}`))
		if err == nil {
			t.Fatalf("accepted conflicting copy destinations: %s", copies)
		}
	}
}
