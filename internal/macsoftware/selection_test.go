package macsoftware

import (
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestApplicationOptionsRequireAnApplication(t *testing.T) {
	facts := plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{
		{ID: ".", Kind: "container", Path: "."},
		{ID: "PackageInfo", Parent: ".", Kind: "package", Path: "PackageInfo", Package: &plugin.PackageFacts{Identifier: "org.example.script", Version: "1"}},
	}}
	if app, err := selectApp(facts, nil); err != nil || app != nil {
		t.Fatalf("script-only package requires no application: %+v, %v", app, err)
	}
	for _, options := range []*Application{{VersionKey: "CFBundleVersion"}, {InstalledPath: "/Applications/Example.app"}} {
		if _, err := selectApp(facts, options); err == nil {
			t.Fatalf("ignored application options without an application: %+v", options)
		}
	}
}

func TestPackageApplicationRequiresExplicitSelection(t *testing.T) {
	for _, installed := range []string{"/Applications/Example.app", "/private/tmp/Example.app", ""} {
		t.Run(installed, func(t *testing.T) {
			subject := plugin.Subject{ID: "Payload/Example.app", Path: "Payload/Example.app", InstalledPath: installed, App: &plugin.AppFacts{BundleID: "org.example.app"}}
			facts := plugin.Facts{Subjects: []plugin.Subject{subject}}
			if app, err := selectApp(facts, nil); err != nil || app != nil {
				t.Fatalf("inferred package application: %+v, %v", app, err)
			}
			for _, options := range []*Application{{}, {VersionKey: "CFBundleVersion"}, {InstalledPath: "/Applications/Example.app"}} {
				if _, err := selectApp(facts, options); err == nil {
					t.Fatalf("accepted package application options without a selector: %+v", options)
				}
			}
			for _, options := range []*Application{{Path: subject.Path}, {BundleID: subject.App.BundleID}} {
				if app, err := selectApp(facts, options); err != nil || app == nil || app.ID != subject.ID {
					t.Fatalf("explicit selection = %+v, %v", app, err)
				}
			}
		})
	}
}

func TestCommandPayloadRequiresAnExplicitPackageLayout(t *testing.T) {
	facts := plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Kind: "file", Path: "."}}}
	for _, traversable := range []bool{false, true} {
		if _, _, err := choose(Spec{}, traversable, facts); err == nil || !strings.Contains(err.Error(), "BuildMacPkg") {
			t.Fatalf("command payload accepted or not explained: %v", err)
		}
	}
}
