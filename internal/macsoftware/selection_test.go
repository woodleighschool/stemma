package macsoftware

import (
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
