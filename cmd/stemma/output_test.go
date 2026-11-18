package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/reconcile"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestFiniteOutputContainsReportsAndWarningsOnly(t *testing.T) {
	var out, logs bytes.Buffer
	cmd, finish := command(&out, &logs, nil)
	cmd.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
		plugin.Stage(cmd.Context(), "Acquiring input")(errors.New("failure detail"))
		plugin.Logger(cmd.Context()).DebugContext(cmd.Context(), "Cache lookup")
		plugin.Logger(cmd.Context()).WarnContext(cmd.Context(), "Verification disabled")
		_, err := io.WriteString(cmd.OutOrStdout(), "report\n")
		return err
	}})
	cmd.SetArgs([]string{"probe"})
	err := cmd.ExecuteContext(t.Context())
	err = finish(err)
	if err != nil || out.String() != "report\n" || logs.String() != "! Warning:\n  Verification disabled\n" {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), logs.String())
	}
}

func TestAcquisitionFailureProducesHonestJSONReport(t *testing.T) {
	project := t.TempDir()
	testproject.Write(t, filepath.Join(project, "stemma.yaml"), `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: fixture}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: missing-installer}
spec:
  source: {path: missing.pkg}
  signatures: [{signer: apple:developer-id:SMLKBTR495}]
`)
	var out, logs bytes.Buffer
	cmd, finish := command(&out, &logs, nil)
	cmd.SetArgs([]string{"update", "--root", project, "--cache-dir", t.TempDir(), "--json"})
	err := cmd.ExecuteContext(t.Context())
	err = finish(err)
	if err == nil {
		t.Fatal("missing input succeeded")
	}
	var report engine.Report
	decoder := json.NewDecoder(&out)
	if err := decoder.Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.LockChanged == nil || *report.LockChanged || report.Error == "" || len(report.Resources) != 1 || report.Resources[0].Error == "" || report.Summary.Failed != 1 {
		t.Fatalf("failure did not report retained lock and failed resource: %+v", report)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatalf("extra stdout: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("stderr=%q", logs.String())
	}
}

func TestOutcomeCommandsPrintTextUnlessAskedForJSON(t *testing.T) {
	run := func(args ...string) string {
		t.Helper()
		var out, logs bytes.Buffer
		cmd, finish := command(&out, &logs, nil)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		err = finish(err)
		if err != nil || logs.Len() != 0 {
			t.Fatalf("%v: %v %s", args, err, logs.String())
		}
		return out.String()
	}
	if got := run("version"); got != "stemma: dev (commit unknown, built unknown)\n" {
		t.Fatalf("version: %q", got)
	}
	var build map[string]string
	if err := json.Unmarshal([]byte(run("version", "--json")), &build); err != nil || build["version"] != "dev" {
		t.Fatalf("version --json: %v %v", build, err)
	}
}

func TestPluginUpdateNamesWhatHappenedToEachEntry(t *testing.T) {
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	update := engine.PluginUpdate{LockChanged: true, Removed: []string{"retired"}, Plugins: []engine.PluginReport{
		{Name: "echo", Path: "plugins/echo", Locked: true, Before: digest("a"), Digest: digest("b")},
		{Name: "kept", Image: "registry.example/kept:dev", Locked: true, Before: digest("c"), Digest: digest("c")},
		{Name: "new", Image: "registry.example/new:dev", Locked: true, Digest: digest("d")},
		{Name: "pinned", Image: "registry.example/pinned:1@" + digest("e"), Digest: digest("e")},
		{Name: "stale", Image: "registry.example/stale:dev", Error: "image tag is not locked; run stemma plugins update"},
	}}
	want := "✓ echo: sha256:aaaaaaaaaaaa → sha256:bbbbbbbbbbbb\n" +
		"✓ kept: unchanged\n✓ new: new sha256:dddddddddddd\n" +
		"✓ pinned: pinned\n✗ stale: failed\n  image tag is not locked; run stemma plugins update\n" +
		"✓ retired: removed (no longer declared)\n" +
		"\n5 plugins: 1 updated, 1 locked, 1 pinned, 1 unchanged, 1 failed.\n"
	if got := pluginUpdate(textStyle{}, update); got != want {
		t.Fatalf("plugins update:\n%s", got)
	}
}

func TestPluginInspectionDescribesCodeAndOfferings(t *testing.T) {
	reports := []engine.PluginReport{
		{
			Name: "downloads", Image: "registry.example/downloads:0.3.0@sha256:" + strings.Repeat("e", 64), Digest: "sha256:" + strings.Repeat("e", 64),
			Version: "0.3.0", Revision: "58d5d19c08d2cbf5cdca2bfd2e658e5f29a130e5", Platforms: []string{"darwin/arm64", "linux/amd64"},
			Operations: []engine.PluginOperation{{Name: "audinate", Kind: "resolve"}, {Name: "blender", Kind: "resolve"}},
		},
		{
			Name: "tools", Path: "plugins/tools", Locked: true, Digest: "sha256:" + strings.Repeat("f", 64), Version: "dev", Revision: "df2cf21a6fa9ee49a9483dc734e9612577cbb891+dirty",
			Operations:  []engine.PluginOperation{{Name: "tools.build", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "example.org/v1", Kind: "VendorPackage"}}},
			Unavailable: []plugin.Unavailable{{Name: "tools.publish", Kind: "reconcile", Reason: "implements reconcile interface 2; this Stemma uses 1"}, {Name: "tools.mirror", Kind: "reconcile", Reason: "implements reconcile interface 2; this Stemma uses 1"}},
		},
	}
	var out bytes.Buffer
	if err := printPlugins(&out, reports); err != nil {
		t.Fatal(err)
	}
	want := `downloads:
  Image: registry.example/downloads:0.3.0@sha256:eeeeeeeeeeee
  Version: 0.3.0 (58d5d19c08d2)
  Platforms: darwin/arm64, linux/amd64
  Resolvers: audinate, blender
tools:
  Path: plugins/tools
  Digest: sha256:ffffffffffff
  Version: dev (df2cf21a6fa9+dirty)
  Resource kinds: example.org/v1/VendorPackage
  ✗ Error:
    tools.publish, tools.mirror: implements reconcile interface 2; this Stemma uses 1
`
	if out.String() != want {
		t.Fatalf("plugins list:\n%s", out.String())
	}
}

func TestMachineReportHoldsEverySelectedResource(t *testing.T) {
	report := engine.Report{Resources: []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "unchanged", Status: engine.StatusUnchanged, Publications: []engine.Publication{{Destination: "repo", Status: engine.StatusUnchanged}}},
		{Kind: "MacSoftware", Name: "changed", Status: engine.StatusChanged, Publications: []engine.Publication{{Destination: "repo", Status: engine.StatusChanged, Version: "2", Previous: "1"}}},
		{Kind: "MacSoftware", Name: "broken", Status: engine.StatusFailed, Error: "invalid signature"},
	}}
	report.Summarize(3)
	var machine bytes.Buffer
	o := newCommandOutput(&machine, io.Discard)
	o.asJSON = true
	for _, resource := range report.Resources {
		if err := o.resourceDone("plan", resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.report(&machine, "plan", report); err != nil {
		t.Fatal(err)
	}
	var decoded engine.Report
	if err := json.Unmarshal(machine.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Resources) != 3 || decoded.Summary.Resources != 3 || decoded.Summary.Unchanged != 1 || decoded.Summary.Failed != 1 || decoded.Resources[1].Publications[0].Previous != "1" {
		t.Fatalf("%+v", decoded)
	}
}

func TestPreparationShowsWhatItBuiltOrCouldNot(t *testing.T) {
	resources := []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "cached", Status: engine.StatusUnchanged, Artifacts: map[string]engine.Prepared{"installer": {Filename: "cached-1.0.pkg", Version: "1.0", Cached: true}}},
		{Kind: "MacSoftware", Name: "built", Status: engine.StatusChanged, Artifacts: map[string]engine.Prepared{"installer": {Filename: "built-2.0.pkg", Version: "2.0"}}},
		{Kind: "MacSoftware", Name: "failed", Status: engine.StatusFailed, Error: "source unavailable"},
		{Kind: "MacSoftware", Name: "blocked", Status: engine.StatusBlocked, BlockedBy: []string{"MacSoftware/failed"}},
	}
	selection := engine.Selection{}
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprintf("interactive=%t", interactive), func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			o := newCommandOutput(&out, &diagnostic)
			o.interactive = interactive
			o.started(selection)
			for _, resource := range resources {
				if err := o.resourceDone("prepare", resource); err != nil {
					t.Fatal(err)
				}
			}
			want := "✓ MacSoftware/cached: 1.0\n  installer: cached-1.0.pkg\n✓ MacSoftware/built: 2.0\n  installer: built-2.0.pkg\n" +
				"✗ MacSoftware/failed: failed\n  source unavailable\n" +
				"✗ MacSoftware/blocked: blocked\n  blocked by MacSoftware/failed\n"
			if out.String() != want || diagnostic.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", &out, &diagnostic)
			}
		})
	}
}

func TestReconcileOutcomesStreamInOrder(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		var out, diagnostic bytes.Buffer
		o := newCommandOutput(&out, &diagnostic)
		o.asJSON, o.reconciling = asJSON, true
		if err := o.applyDone(reconcile.Report{Branch: "main", Head: "123456789abc", Apply: &reconcile.Apply{Skipped: true}}); err != nil {
			t.Fatal(err)
		}
		if err := o.proposalDone(reconcile.Proposal{Name: "MacSoftware/example", Action: "created", PullRequest: "https://example.test/pull/1"}); err != nil {
			t.Fatal(err)
		}
		want := "✓ main@123456789abc: already applied\n" +
			"✓ MacSoftware/example: created\n  pull request: https://example.test/pull/1\n"
		if asJSON {
			want = ""
		}
		if out.String() != want || diagnostic.Len() != 0 {
			t.Fatalf("json=%t stdout=%q stderr=%q", asJSON, &out, &diagnostic)
		}
	}
}

func TestFailedPublicationKeepsItsRowBesideTheOthers(t *testing.T) {
	resource := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Status: engine.StatusFailed, Error: "second: upload failed", Publications: []engine.Publication{
		{Destination: "first", Status: engine.StatusChanged, Version: "2", Previous: "2", Fields: []plugin.FieldChange{{Field: "description", Before: json.RawMessage(`"old"`), After: json.RawMessage(`"new"`)}}},
		{Destination: "second", Status: engine.StatusFailed, Error: "upload failed", Fields: []plugin.FieldChange{{Field: "description", Before: json.RawMessage(`"old"`), After: json.RawMessage(`"new"`)}}},
	}}
	report := engine.Report{Resources: []engine.ResourceReport{resource}}
	report.Summarize(1)
	got := render(t, "apply", engine.Selection{}, report, report.Resources)
	want := "✗ MacSoftware/example:\n  first: 2\n    description: old → new\n" +
		"  second: failed\n    upload failed\n    description: old → new\n" +
		"\n1 resource: 1 failed.\n"
	if got != want {
		t.Fatalf("apply report:\n%q\nwant:\n%q", got, want)
	}
}

func TestFinalErrorShowsOnlyWhatNoReportShowed(t *testing.T) {
	resource := engine.ResourceError{Resource: "MacSoftware/example", Err: errors.New("upload failed")}
	for _, test := range []struct {
		err  error
		want string
	}{
		{errors.Join(resource, errors.New("lock: schema violations:\n\tspec.source: required")), "✗ Error:\n  lock: schema violations:\n      spec.source: required\n"},
		{resource, ""},
		{reconcile.ErrFailed, ""},
		// A report that could not be written showed nothing.
		{errors.Join(reconcile.ErrFailed, errors.New("write stdout: broken pipe")), "✗ Error:\n  reconcile: a phase failed\n    write stdout: broken pipe\n"},
	} {
		var logs bytes.Buffer
		_ = newCommandOutput(io.Discard, &logs).finish(test.err)
		if logs.String() != test.want {
			t.Errorf("finish(%q) printed %q, want %q", test.err, logs.String(), test.want)
		}
	}
	wrapped := fmt.Errorf("project: %w", errors.Join(errors.New("source missing"), errors.New("icon missing")))
	if got := commandError(wrapped); got != "project: source missing\nicon missing" {
		t.Fatalf("lost enclosing context: %s", got)
	}
	// A command that prints only a path shows no report of its resource.
	var logs bytes.Buffer
	o := newCommandOutput(io.Discard, &logs)
	o.resultOnly = true
	_ = o.finish(errors.Join(engine.ResourceError{Resource: "stemma/v1alpha1/MacSoftware/example", Err: errors.New("upload failed")}))
	if want := "✗ Error:\n  MacSoftware/example: upload failed\n"; logs.String() != want {
		t.Fatalf("path-only failure printed %q, want %q", logs.String(), want)
	}
}

func TestStartupFailureDoesNotFabricateReport(t *testing.T) {
	var out, logs bytes.Buffer
	cmd, finish := command(&out, &logs, nil)
	cmd.SetArgs([]string{"plan", "--json", "--root", t.TempDir()})
	err := cmd.ExecuteContext(t.Context())
	err = finish(err)
	if err == nil || out.Len() != 0 || !strings.HasPrefix(logs.String(), "✗ ") {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), logs.String())
	}
}

func TestWarningsAreInsideMachineReport(t *testing.T) {
	var out, logs bytes.Buffer
	o := newCommandOutput(&out, &logs)
	o.asJSON = true
	logger := slog.New(&activityHandler{output: o})
	logger.Warn("Provider warning", "resource", "MacSoftware/example")
	if err := o.report(&out, "plan", engine.Report{Resources: []engine.ResourceReport{}}); err != nil {
		t.Fatal(err)
	}
	_ = o.finish(nil)
	var report engine.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 1 || logs.Len() != 0 {
		t.Fatalf("report=%+v stderr=%q", report, logs.String())
	}
}

type failingReportWriter struct{}

func (failingReportWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestReportWriteFailureRetainsUnpublishedResourceError(t *testing.T) {
	project := t.TempDir()
	testproject.Write(t, filepath.Join(project, "stemma.yaml"), `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: fixture}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: missing-installer}
spec:
  source: {path: missing.pkg}
  signatures: [{signer: apple:developer-id:SMLKBTR495}]
`)
	var stderr bytes.Buffer
	cmd, finish := command(failingReportWriter{}, &stderr, nil)
	cmd.SetArgs([]string{"update", "--root", project, "--cache-dir", t.TempDir(), "--json"})
	err := cmd.ExecuteContext(t.Context())
	if err = finish(err); err == nil {
		t.Fatal("report write failure succeeded")
	}
	for _, want := range []string{"output unavailable", "missing-installer", "missing.pkg"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("unpublished error lost %q: %s", want, stderr.String())
		}
	}
}

func TestSignatureRowsKeepInputsApartFromPublishedSignatures(t *testing.T) {
	for _, test := range []struct {
		observation signature.Observation
		want        []string
	}{
		{signature.Observation{Subject: plugin.SubjectSelector{Path: "."}, State: "signed", Signer: "apple:developer-id:UBF8T346G9", Name: "Microsoft Corporation", Authority: "Developer ID Installer"}, []string{"Microsoft Corporation (Developer ID Installer)", "  .: Microsoft Corporation", "signatures:", "- signer: apple:developer-id:UBF8T346G9"}},
		{signature.Observation{Subject: plugin.SubjectSelector{Path: "Store.app"}, State: "signed", Signer: "apple:app-store:ABCDE12345", Authority: "Mac App Store"}, []string{"Mac App Store\n", "- signer: apple:app-store:ABCDE12345"}},
		{signature.Observation{Input: "vendor", Subject: plugin.SubjectSelector{Path: "Install.app"}, State: "unsigned"}, []string{"unsigned\n", "vendor/Install.app:", `input: "vendor"`, "unsigned: true"}},
	} {
		evidence, err := json.Marshal([]signature.Observation{test.observation})
		if err != nil {
			t.Fatal(err)
		}
		resource := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Status: engine.StatusChanged, Artifacts: map[string]engine.Prepared{"installer": {Evidence: map[string]json.RawMessage{"signing": json.RawMessage(`{"observed":` + string(evidence) + `}`)}}}}
		got := renderRows(textStyle{}, resourceName(resource), resource.Status, resourceRows("signature", resource, false))
		for _, want := range test.want {
			if !strings.Contains(got, want) {
				t.Fatalf("report lost %q: %s", want, got)
			}
		}
	}
}

func TestCancellationWithoutInterruptIsFailure(t *testing.T) {
	var logs bytes.Buffer
	output := newCommandOutput(io.Discard, &logs)
	_ = output.finish(context.Canceled)
	if logs.String() != "✗ Error:\n  context canceled\n" {
		t.Fatal(logs.String())
	}
}

func TestResourceNoticesAppearOnceForUnchangedResources(t *testing.T) {
	resource := engine.ResourceReport{Kind: "MacSoftware", Name: "foo", Status: engine.StatusUnchanged, Publications: []engine.Publication{{Destination: "repo", Status: engine.StatusUnchanged}}, Notices: []plugin.Notice{
		{Level: "warning", Code: "signature-expectation-missing", Message: "Source has no signature expectation", Hint: "Run `stemma signature MacSoftware/foo` to derive one."},
		{Level: "warning", Code: "signature-timestamp-missing", Message: "Developer ID signature has no secure timestamp; certificate validity at signing time cannot be established"},
	}}
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", asJSON), func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			output := newCommandOutput(&out, &diagnostic)
			output.asJSON = asJSON
			// Activity stays disabled, as with --no-progress.
			if err := output.resourceDone("plan", resource); err != nil {
				t.Fatal(err)
			}
			if asJSON && out.Len() != 0 {
				t.Fatalf("JSON resource streamed: %s", &out)
			}
			// A later reconcile phase must not repeat the same recommendation.
			if err := output.resourceDone("plan", resource); err != nil {
				t.Fatal(err)
			}
			report := engine.Report{Resources: []engine.ResourceReport{resource}}
			report.Summarize(1)
			if err := output.report(&out, "plan", report); err != nil {
				t.Fatal(err)
			}
			_ = output.finish(nil)
			if asJSON {
				var decoded engine.Report
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				if diagnostic.Len() != 0 || len(decoded.Resources[0].Notices) != 2 {
					t.Fatalf("report=%s stderr=%s", &out, &diagnostic)
				}
			} else if want := "! MacSoftware/foo:\n  Source has no signature expectation\n  Run `stemma signature MacSoftware/foo` to derive one.\n" +
				"! MacSoftware/foo:\n  Developer ID signature has no secure timestamp; certificate validity at signing time cannot be\n  established\n"; diagnostic.String() != want {
				t.Fatalf("stderr=%q want=%q", diagnostic.String(), want)
			}
		})
	}
}
