package changes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// plain joins a row's semantic text for assertions.
func plain(rows []Row) string {
	var text strings.Builder
	for _, row := range rows {
		if row.Subject != "" {
			text.WriteString(row.Subject + ": ")
		}
		text.WriteString(row.Summary.String() + "\n")
		for _, detail := range row.Details {
			text.WriteString("  " + detail.String() + "\n")
		}
	}
	return text.String()
}

func entry(version, filename, digest, url string) *source.Entry {
	return &source.Entry{
		Version:         version,
		Content:         source.Content{Filename: filename, SHA256: strings.Repeat(digest, 64), Mode: 0o644},
		Origin:          json.RawMessage(`{"url":"` + url + `"}`),
		Resolver:        "homebrew",
		ResolverVersion: "3",
		Declaration:     strings.Repeat("d", 64),
	}
}

func TestInputRowsLeadWithTheChange(t *testing.T) {
	versioned := entry("3.22.6", "UnityHubSetup-3.22.6-arm64.dmg", "e", "https://cdn.test/3.22.6/UnityHubSetup-3.22.6-arm64.dmg")
	bumped := entry("4.0.0", "UnityHubSetup-4.0.0-arm64.dmg", "2", "https://cdn.test/4.0.0/UnityHubSetup-4.0.0-arm64.dmg")
	static := entry("", "FXMathManifest.pkg", "9", "https://vendor.test/FXMathManifest.pkg")
	rebuilt := entry("", "FXMathManifest.pkg", "5", "https://vendor.test/FXMathManifest.pkg")
	renamedBefore := entry("", "MicrosoftEdge-157.0.4341.46.pkg", "a", "https://vendor.test/edge")
	renamedAfter := entry("", "MicrosoftEdge-157.0.4341.60.pkg", "b", "https://vendor.test/edge")
	sameVersion := entry("1.0", "app.pkg", "c", "https://vendor.test/app.pkg")
	reissued := entry("1.0", "app.pkg", "f", "https://vendor.test/app.pkg")
	refreshed := *versioned
	refreshed.Evidence = map[string]json.RawMessage{"homebrew": json.RawMessage(`{"cask":{"macos":"13"}}`)}
	versioned.Evidence = map[string]json.RawMessage{"homebrew": json.RawMessage(`{"cask":{"macos":"12"}}`)}
	fullVersion := *versioned
	fullVersion.Version += ",12345"
	for name, test := range map[string]struct {
		change lock.InputChange
		want   string
	}{
		"version":            {lock.InputChange{Kind: lock.Updated, Before: versioned, After: bumped}, "3.22.6 → 4.0.0\n"},
		"no version":         {lock.InputChange{Kind: lock.Updated, Before: static, After: rebuilt}, "FXMathManifest.pkg file changed\n"},
		"renamed":            {lock.InputChange{Kind: lock.Updated, Before: renamedBefore, After: renamedAfter}, "MicrosoftEdge-157.0.4341.46.pkg → MicrosoftEdge-157.0.4341.60.pkg\n"},
		"same version":       {lock.InputChange{Kind: lock.Updated, Before: sameVersion, After: reissued}, "1.0 file changed\n"},
		"added":              {lock.InputChange{Kind: lock.Added, After: bumped}, "new 4.0.0\n"},
		"removed":            {lock.InputChange{Input: "orbit", Kind: lock.Removed, Before: versioned}, "orbit removed\n"},
		"refreshed evidence": {lock.InputChange{Kind: lock.Refreshed, Before: versioned, After: &refreshed}, "3.22.6\n  evidence.homebrew.cask.macos: 12 → 13\n"},
		"refreshed version":  {lock.InputChange{Kind: lock.Refreshed, Before: versioned, After: &fullVersion}, "3.22.6 → 3.22.6,12345\n"},
	} {
		got := plain(InputRows([]lock.InputChange{test.change}, false))
		if !strings.HasPrefix(got, test.want) {
			t.Errorf("%s: got %q want %q", name, got, test.want)
		}
		if strings.Contains(got, "\n  version:") {
			t.Errorf("%s repeated the version below its headline: %s", name, got)
		}
	}
	got := plain(InputRows([]lock.InputChange{{Input: "orbit", Kind: lock.Updated, Before: versioned, After: bumped}}, true))
	want := "orbit: 3.22.6 → 4.0.0\n" +
		"  content.filename: UnityHubSetup-3.22.6-arm64.dmg → UnityHubSetup-4.0.0-arm64.dmg\n" +
		"  content.sha256: eeeeeeeeeeee… → 222222222222…\n" +
		"  evidence (removed):\n" +
		"  origin.url: https://cdn.test/3.22.6/UnityHubSetup-3.22.6-arm64.dmg → https://cdn.test/4.0.0/UnityHubSetup-4.0.0-arm64.dmg\n"
	if !strings.HasPrefix(got, "orbit: 3.22.6 → 4.0.0\n  content.filename") || !strings.Contains(got, "origin.url: https://cdn.test/3.22.6") || strings.Contains(got, "version:") {
		t.Errorf("details:\n%s\nwant like:\n%s", got, want)
	}
}

func installer(before, after string) []plugin.Upload {
	return []plugin.Upload{{Name: plugin.UploadInstaller, Filename: "app.pkg", Before: before, After: after}}
}

func TestPublicationRowsStateTheVersionClientsGet(t *testing.T) {
	field := func(name, before, after string) plugin.FieldChange {
		change := plugin.FieldChange{Field: name}
		if before != "" {
			change.Before = json.RawMessage(before)
		}
		if after != "" {
			change.After = json.RawMessage(after)
		}
		return change
	}
	for name, test := range map[string]struct {
		publication engine.Publication
		want        string
	}{
		"version bump retiring the version it replaces": {
			engine.Publication{Destination: "woodstar", Version: "26.1111.21308", Previous: "26.1106.51428", Uploads: installer("a", "b"), Removed: []string{"26.1106.51428"}, Fields: []plugin.FieldChange{field("pkginfo.installed_size", "100", "120")}},
			"woodstar: 26.1106.51428 → 26.1111.21308\n  pkginfo.installed_size: 100 → 120\n  upload installer: app.pkg\n    digest: a → b\n  remove 26.1106.51428\n",
		},
		"new bytes at the same version": {
			engine.Publication{Destination: "woodstar", Version: "25.10.26", Previous: "25.10.26", Uploads: installer("a", "b"), Fields: []plugin.FieldChange{field("pkginfo.installer_item_size", "1", "2")}},
			"woodstar: 25.10.26\n  pkginfo.installer_item_size: 1 → 2\n  upload installer: app.pkg\n    digest: a → b\n",
		},
		"bytes the destination lost": {
			engine.Publication{Destination: "munki", Version: "1.0", Previous: "1.0", Uploads: installer("a", "a")},
			"munki: 1.0\n  upload installer: app.pkg\n    digest: a → a\n",
		},
		"new software shows initial values": {
			engine.Publication{Destination: "woodstar", Version: "1.9.10", Uploads: installer("", "b"), Origins: map[string]string{"pkginfo.category": "explicit", "pkginfo.installed_size": "artifact.installed_size"}, Fields: []plugin.FieldChange{field("pkginfo.category", "", `"Productivity"`), field("pkginfo.installed_size", "", "120")}},
			"woodstar: new 1.9.10\n  pkginfo.category: Productivity\n  pkginfo.installed_size: 120\n  upload installer: app.pkg\n    digest: b (previous digest unavailable)\n",
		},
		"retention beyond the replaced version": {
			engine.Publication{Destination: "woodstar", Version: "7.43.2", Previous: "7.43.2", Removed: []string{"7.41.0", "7.42.0"}},
			"woodstar: 7.43.2\n  remove 7.41.0, 7.42.0\n",
		},
		"a field change beneath its headline": {
			engine.Publication{Destination: "intune", Version: "7.3", Previous: "7.3", Fields: []plugin.FieldChange{field("description", `"Old"`, `"New"`)}},
			"intune: 7.3\n  description: Old → New\n",
		},
		"content fields when nothing else explains the change": {
			engine.Publication{Destination: "woodstar", Version: "157.0", Previous: "157.0", Fields: []plugin.FieldChange{field("pkginfo.installs", `[{"path":"/Applications/App.app","CFBundleVersion":"1"}]`, `[{"path":"/Applications/App.app","CFBundleVersion":"2"}]`)}},
			"woodstar: 157.0\n  pkginfo.installs:\n    - /Applications/App.app: CFBundleVersion: 1\n    + /Applications/App.app: CFBundleVersion: 2\n",
		},
		"failure": {
			engine.Publication{Destination: "jamf", Error: "401 Unauthorized\nthe client secret was rejected"},
			"jamf: failed\n  401 Unauthorized\n  the client secret was rejected\n",
		},
		"destination without versions": {
			engine.Publication{Destination: "custom", Fields: []plugin.FieldChange{field("config", `{}`, `{"a":1}`)}},
			"custom: updated\n  config.a: (absent) → 1\n",
		},
	} {
		if got := plain([]Row{PublicationRow(test.publication)}); got != test.want {
			t.Errorf("%s: got %q want %q", name, got, test.want)
		}
	}
	details := plain([]Row{PublicationRow(engine.Publication{Destination: "woodstar", Version: "2", Previous: "1", Uploads: installer(strings.Repeat("a", 64), strings.Repeat("b", 64)), Fields: []plugin.FieldChange{field("pkginfo.installed_size", "100", "120")}})})
	if !strings.Contains(details, "pkginfo.installed_size: 100 → 120") || !strings.Contains(details, "digest: aaaaaaaaaaaa… → bbbbbbbbbbbb…") {
		t.Errorf("details hide content fields or digests:\n%s", details)
	}
}

func TestSummaryCountsResourcesInOneGrammar(t *testing.T) {
	for _, test := range []struct {
		method  string
		summary engine.Summary
		want    string
	}{
		{"update", engine.Summary{Resources: 103, Changed: 1, Unchanged: 102}, "103 resources: 1 updated, 102 unchanged."},
		{"apply", engine.Summary{Resources: 103, Changed: 8, Unchanged: 95, Removed: 7}, "103 resources: 8 published, 95 unchanged; 7 older versions removed."},
		{"plan", engine.Summary{Resources: 103, Changed: 4, Unchanged: 98, Failed: 1}, "103 resources: 4 to publish, 98 unchanged, 1 failed."},
		{"update", engine.Summary{Resources: 103, Changed: 7, Unchanged: 94, Failed: 1, Blocked: 1}, "103 resources: 7 updated, 94 unchanged, 1 failed, 1 blocked."},
		{"prepare", engine.Summary{Resources: 103, Changed: 4, Unchanged: 99}, "103 resources: 4 prepared, 99 cached."},
		{"apply", engine.Summary{Resources: 103, Changed: 3, Unchanged: 40, NotReached: 60}, "103 resources: 3 published, 40 unchanged, 60 not reached."},
	} {
		if got := Summary(test.method, test.summary).String(); got != test.want {
			t.Errorf("%s: got %q want %q", test.method, got, test.want)
		}
	}
}
