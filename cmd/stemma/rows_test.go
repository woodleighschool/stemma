package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// run streams resources through the human report as a command does and
// returns what it printed.
func render(t *testing.T, method string, selection engine.Selection, report engine.Report, streamed []engine.ResourceReport) string {
	t.Helper()
	var out bytes.Buffer
	o := newCommandOutput(&out, io.Discard)
	o.started(selection)
	for _, resource := range streamed {
		if err := o.resourceDone(method, resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.report(&out, method, report); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestUpdateRunPrintsTheVersionChange(t *testing.T) {
	entry := func(version, filename, digest string) *source.Entry {
		return &source.Entry{Version: version, Content: source.Content{Filename: filename, SHA256: strings.Repeat(digest, 64), Mode: 0o644}, Resolver: "homebrew", ResolverVersion: "3", Declaration: strings.Repeat("d", 64)}
	}
	changed := engine.ResourceReport{Kind: "MacSoftware", Name: "unity-hub", Status: engine.StatusChanged, Inputs: []lock.InputChange{{Input: "source", Kind: lock.Updated, Before: entry("3.22.6", "UnityHubSetup-3.22.6-arm64.dmg", "e"), After: entry("4.0.0", "UnityHubSetup-4.0.0-arm64.dmg", "2")}}}
	report := engine.Report{Resources: []engine.ResourceReport{changed}}
	report.Resources = append(report.Resources, engine.ResourceReport{Kind: "MacSoftware", Name: "current", Status: engine.StatusUnchanged})
	changedTrue := true
	report.LockChanged = &changedTrue
	report.Summarize(len(report.Resources))
	got := render(t, "update", engine.Selection{}, report, []engine.ResourceReport{changed})
	for _, want := range []string{"✓ MacSoftware/unity-hub: 3.22.6 → 4.0.0\n", "content.sha256:", "content.filename:", "2 resources: 1 updated, 1 unchanged."} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	got = render(t, "update", engine.Selection{NamedInputs: []string{"MacSoftware/unity-hub"}}, report, []engine.ResourceReport{changed})
	if !strings.Contains(got, "✓ MacSoftware/unity-hub:\n  source: 3.22.6 → 4.0.0\n    content.filename:") {
		t.Fatalf("one changed input lost its scope in a resource with multiple inputs: %s", got)
	}
}

func TestInterruptedRunCountsWhatItNeverReached(t *testing.T) {
	var out, logs bytes.Buffer
	o := newCommandOutput(&out, &logs)
	o.started(engine.Selection{})
	applied := engine.ResourceReport{Kind: "MacSoftware", Name: "done", Status: engine.StatusChanged, Publications: []engine.Publication{{Destination: "repo", Status: engine.StatusChanged, Version: "2", Previous: "1"}}}
	if err := o.resourceDone("apply", applied); err != nil {
		t.Fatal(err)
	}
	report := engine.Report{Resources: []engine.ResourceReport{applied}}
	report.Summarize(2)
	if err := o.report(&out, "apply", report); err != nil {
		t.Fatal(err)
	}
	_ = o.finish(errInterrupted)
	want := "✓ MacSoftware/done:\n  repo: 1 → 2\n\n2 resources: 1 published, 1 not reached.\n"
	if out.String() != want || logs.String() != "– Interrupted.\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), logs.String())
	}
}

func TestRowsShowEveryResourceOutcome(t *testing.T) {
	report := engine.Report{Resources: []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "current", Status: engine.StatusUnchanged, Publications: []engine.Publication{{Destination: "repo", Status: engine.StatusUnchanged, Version: "1", Previous: "1"}}},
		{Kind: "MacSoftware", Name: "broken", Status: engine.StatusFailed, Error: "MacSoftware/broken: invalid signature"},
		{Kind: "MacSoftware", Name: "waiting", Status: engine.StatusBlocked, BlockedBy: []string{"stemma/v1alpha1/MacSoftware/broken"}},
	}}
	report.Summarize(3)
	selection := engine.Selection{}
	got := render(t, "plan", selection, report, report.Resources)
	want := "✓ MacSoftware/current: unchanged (repo 1)\n✗ MacSoftware/broken: failed\n  invalid signature\n" +
		"✗ MacSoftware/waiting: blocked\n  blocked by MacSoftware/broken\n" +
		"\n3 resources: 1 unchanged, 1 failed, 1 blocked.\n"
	if got != want {
		t.Fatalf("plan report:\n%q\nwant:\n%q", got, want)
	}
	var out bytes.Buffer
	o := newCommandOutput(&out, io.Discard)
	o.started(selection)
	if err := o.resourceDone("plan", report.Resources[0]); err != nil {
		t.Fatal(err)
	}
	if out.String() != "✓ MacSoftware/current: unchanged (repo 1)\n" {
		t.Fatalf("unchanged row = %q", out.String())
	}
}

func TestPlanAndApplyUseTheSameRows(t *testing.T) {
	resource := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Status: engine.StatusChanged, Publications: []engine.Publication{
		{Destination: "repo", Version: "2", Previous: "1", Status: engine.StatusChanged,
			Uploads: []plugin.Upload{{Name: plugin.UploadInstaller, Filename: "example-2.pkg", Before: "a", After: "b"}},
			Removed: []string{"1"}},
		{Destination: "intune", Version: "2", Previous: "2", Status: engine.StatusChanged,
			Fields:  []plugin.FieldChange{{Field: "included_apps", Before: json.RawMessage(`[{"id":"com.example.old","version":"1"}]`), After: json.RawMessage(`[{"id":"com.example.app","version":"2"}]`)}},
			Uploads: []plugin.Upload{{Name: plugin.UploadIcon, Filename: "icon.png", After: "c"}}},
	}}
	plan := resourceRows("plan", resource, false)
	apply := resourceRows("apply", resource, false)
	got := renderRows(textStyle{}, resourceName(resource), resource.Status, plan)
	if other := renderRows(textStyle{}, resourceName(resource), resource.Status, apply); got != other {
		t.Fatalf("plan and apply differ:\n%s\n%s", got, other)
	}
	for _, want := range []string{"remove 1", "- com.example.old", "+ com.example.app", "upload installer: example-2.pkg", "upload icon: icon.png", "digest: a → b"} {
		if !strings.Contains(got, want) {
			t.Fatalf("change missing %q: %s", want, got)
		}
	}
}
