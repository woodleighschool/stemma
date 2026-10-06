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
	cmd, finish := command(&out, &logs)
	cmd.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
		plugin.Stage(cmd.Context(), "Acquiring input")(errors.New("failure detail"))
		plugin.Logger(cmd.Context()).DebugContext(cmd.Context(), "Cache lookup")
		plugin.Logger(cmd.Context()).WarnContext(cmd.Context(), "Verification disabled")
		_, err := io.WriteString(cmd.OutOrStdout(), "report\n")
		return err
	}})
	cmd.SetArgs([]string{"probe"})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	if err != nil || out.String() != "report\n" || logs.String() != "Warning: Verification disabled\n" {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), logs.String())
	}
}

func TestFiniteCommandsRejectLoggingFlags(t *testing.T) {
	for _, flag := range []string{"--log-level", "--log-format", "--quiet", "--verbose", "--debug"} {
		var out, logs bytes.Buffer
		cmd, finish := command(&out, &logs)
		cmd.SetArgs([]string{"plan", flag})
		err := cmd.ExecuteContext(t.Context())
		finish(err)
		if err == nil || !strings.Contains(err.Error(), "unknown flag") || out.Len() != 0 {
			t.Fatalf("%s: %v, stdout=%q", flag, err, out.String())
		}
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
	cmd, finish := command(&out, &logs)
	cmd.SetArgs([]string{"update", "--root", project, "--cache-dir", t.TempDir(), "--json"})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
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
	if !strings.HasPrefix(logs.String(), "Error: MacSoftware/missing-installer: input source: open ") || strings.Count(logs.String(), "\n") != 1 {
		t.Fatalf("stderr=%q", logs.String())
	}
}

func TestOutcomeCommandsPrintTextUnlessAskedForJSON(t *testing.T) {
	run := func(args ...string) string {
		t.Helper()
		var out, logs bytes.Buffer
		cmd, finish := command(&out, &logs)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		finish(err)
		if err != nil || logs.Len() != 0 {
			t.Fatalf("%v: %v %s", args, err, logs.String())
		}
		return out.String()
	}
	if got := run("version"); got != "stemma dev (commit unknown, built unknown)\n" {
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
	var out bytes.Buffer
	if err := printPluginUpdate(&out, update); err != nil {
		t.Fatal(err)
	}
	want := "echo: updated\n  sha256:aaaaaaaaaaaa -> sha256:bbbbbbbbbbbb\nkept: unchanged\nnew: locked\n  sha256:dddddddddddd\npinned: pinned\nstale: failed\n  ✗ image tag is not locked; run stemma plugins update\nretired: no longer declared\nLockfile updated.\n"
	if out.String() != want {
		t.Fatalf("plugins update:\n%s", out.String())
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
	want := `downloads
  Image:      registry.example/downloads:0.3.0@sha256:eeeeeeeeeeee
  Version:    0.3.0 (58d5d19c08d2)
  Platforms:  darwin/arm64, linux/amd64
  Resolvers:  audinate, blender

tools
  Path:            plugins/tools
  Digest:          sha256:ffffffffffff
  Version:         dev (df2cf21a6fa9+dirty)
  Resource kinds:  example.org/v1/VendorPackage
  ✗ tools.publish, tools.mirror: implements reconcile interface 2; this Stemma uses 1
`
	if out.String() != want {
		t.Fatalf("plugins list:\n%s", out.String())
	}
}

func TestHumanReportsStreamSelectedResourcesAndTotalTheRun(t *testing.T) {
	report := engine.Report{Resources: []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "unchanged", Destinations: []engine.DestinationReport{{Name: "repo"}}},
		{Kind: "MacSoftware", Name: "changed", Destinations: []engine.DestinationReport{{Name: "repo", Changes: []plugin.Change{{Action: "set", Field: "package.version", Before: json.RawMessage(`"1"`), After: json.RawMessage(`"2"`)}}}}},
		{Kind: "MacSoftware", Name: "broken", Error: "invalid signature"},
	}}
	report.Summarize("plan")
	for _, all := range []bool{false, true} {
		var human, machine bytes.Buffer
		o := newCommandOutput(&human, io.Discard)
		o.all = all
		for _, resource := range report.Resources {
			if err := o.resourceDone("plan", resource); err != nil {
				t.Fatal(err)
			}
		}
		streamed := human.String()
		for _, want := range []string{"➤ MacSoftware/changed\n  repo\n    → package.version: 1 -> 2\n", "➤ MacSoftware/broken · failed\n  ✗ invalid signature\n"} {
			if !strings.Contains(streamed, want) {
				t.Fatalf("all=%v: block %q did not stream: %s", all, want, streamed)
			}
		}
		if strings.Contains(streamed, "MacSoftware/unchanged") != all {
			t.Fatalf("all=%v: unchanged resource: %s", all, streamed)
		}
		if err := o.report(&human, "plan", report, nil); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimPrefix(human.String(), streamed); got != "Plan: 1 resource with changes, 1 destination, 1 unchanged, 1 failed.\n" {
			t.Fatalf("all=%v: summary %q", all, got)
		}
		o = newCommandOutput(&machine, io.Discard)
		o.asJSON, o.all = true, all
		for _, resource := range report.Resources {
			if err := o.resourceDone("plan", resource); err != nil {
				t.Fatal(err)
			}
		}
		if err := o.report(&machine, "plan", report, nil); err != nil {
			t.Fatal(err)
		}
		var decoded engine.Report
		if err := json.Unmarshal(machine.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		want := 3
		if len(decoded.Resources) != want || decoded.Summary.Resources != 3 || decoded.Summary.Unchanged != 1 || decoded.Summary.Failed != 1 {
			t.Fatalf("all=%v: %+v", all, decoded)
		}
	}
}

func TestApplyReportDoesNotConfirmFailedDestinationChanges(t *testing.T) {
	report := engine.Report{Error: "upload failed", Resources: []engine.ResourceReport{{Kind: "MacSoftware", Name: "example", Error: "upload failed", Destinations: []engine.DestinationReport{
		{Name: "first", Applied: true, Changes: []plugin.Change{{Action: "set", Field: "description", Before: json.RawMessage(`"old"`), After: json.RawMessage(`"new"`)}}},
		{Name: "second", Error: "upload failed", Changes: []plugin.Change{{Action: "upload", Field: "installer", After: json.RawMessage(`"payload"`)}}},
	}}}}
	report.Summarize("apply")
	text := renderResourceDetail(textStyle{}, "apply", report.Resources[0], false) + renderSummary(textStyle{}, "apply", report, errors.New("upload failed"))
	for _, want := range []string{"➤ MacSoftware/example\n", "  first\n    ✓ Updated 1 metadata field\n", "  second · failed (changes not confirmed)\n", "    ✗ upload failed\n", "Apply incomplete: 1 publication applied; 1 resource, 2 destinations", "1 failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Count(text, "upload failed") != 1 {
		t.Fatalf("repeated failure: %s", text)
	}
}

func TestInterruptedRunKeepsStreamedResultsAndSaysSo(t *testing.T) {
	var out, logs bytes.Buffer
	o := newCommandOutput(&out, &logs)
	applied := engine.ResourceReport{Kind: "MacSoftware", Name: "done", Destinations: []engine.DestinationReport{{Name: "repo", Applied: true, Changes: []plugin.Change{{Action: "set", Field: "description"}}}}}
	if err := o.resourceDone("apply", applied); err != nil {
		t.Fatal(err)
	}
	report := engine.Report{Error: context.Canceled.Error(), Resources: []engine.ResourceReport{applied}}
	report.Summarize("apply")
	if err := o.report(&out, "apply", report, errInterrupted); err != nil {
		t.Fatal(err)
	}
	o.finish(errInterrupted)
	if !strings.HasPrefix(out.String(), "➤ MacSoftware/done\n") || !strings.HasSuffix(out.String(), "Apply interrupted: 1 publication applied; 1 resource, 1 destination; 0 unchanged.\n") || logs.String() != "Interrupted.\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), logs.String())
	}
}

func TestFinalErrorShowsOnlyWhatNoReportShowed(t *testing.T) {
	resource := engine.ResourceError{Resource: "MacSoftware/example", Err: errors.New("upload failed")}
	for _, test := range []struct {
		err  error
		want string
	}{
		{errors.Join(resource, errors.New("lockfile: schema violations:\n\tspec.source: required")), "Error: lockfile: schema violations:\n    spec.source: required\n"},
		{resource, "Error: MacSoftware/example: upload failed\n"},
		{reconcile.ErrFailed, "Error: reconcile: a phase failed\n"},
		// A report that could not be written showed nothing.
		{errors.Join(reconcile.ErrFailed, errors.New("write stdout: broken pipe")), "Error: reconcile: a phase failed\n  write stdout: broken pipe\n"},
	} {
		var logs bytes.Buffer
		newCommandOutput(io.Discard, &logs).finish(test.err)
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
	o.finish(errors.Join(engine.ResourceError{Resource: "stemma/v1alpha1/MacSoftware/example", Err: errors.New("upload failed")}))
	if want := "Error: MacSoftware/example: upload failed\n"; logs.String() != want {
		t.Fatalf("path-only failure printed %q, want %q", logs.String(), want)
	}
}

func TestStartupFailureDoesNotFabricateReport(t *testing.T) {
	var out, logs bytes.Buffer
	cmd, finish := command(&out, &logs)
	cmd.SetArgs([]string{"plan", "--json", "--root", t.TempDir()})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	if err == nil || out.Len() != 0 || !strings.HasPrefix(logs.String(), "Error: ") {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), logs.String())
	}
}

func TestWarningsAreInsideMachineReport(t *testing.T) {
	var out, logs bytes.Buffer
	o := newCommandOutput(&out, &logs)
	o.asJSON = true
	logger := slog.New(&activityHandler{output: o})
	logger.Warn("Provider warning", "resource", "MacSoftware/example")
	if err := o.report(&out, "plan", engine.Report{Resources: []engine.ResourceReport{}}, nil); err != nil {
		t.Fatal(err)
	}
	o.finish(nil)
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
	cmd, finish := command(failingReportWriter{}, &stderr)
	cmd.SetArgs([]string{"update", "--root", project, "--cache-dir", t.TempDir(), "--json"})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	for _, want := range []string{"output unavailable", "missing-installer", "missing.pkg"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("unpublished error lost %q: %s", want, stderr.String())
		}
	}
}

func TestSignatureDetailsKeepInputsApartFromPublishedSignatures(t *testing.T) {
	for _, test := range []struct {
		observation signature.Observation
		want        []string
	}{
		{signature.Observation{Subject: plugin.SubjectSelector{Path: "."}, State: "signed", Signer: "apple:developer-id:UBF8T346G9", Name: "Microsoft Corporation", Authority: "Developer ID Installer"}, []string{"Subject: .", "Signer: Microsoft Corporation (Developer ID Installer)", "signatures:", "- signer: apple:developer-id:UBF8T346G9"}},
		{signature.Observation{Subject: plugin.SubjectSelector{Path: "Example.app"}, State: "signed", Signer: "apple:developer-id:ABCDE12345", Name: "Example", Authority: "Developer ID Application", Replaced: []signature.Replacement{{Path: "Contents/Frameworks/helper.so", Sealed: "aa", CDHashes: []string{"bb"}}}}, []string{"Signer: Example (Developer ID Application)", "Replaced nested code: Contents/Frameworks/helper.so"}},
		{signature.Observation{Subject: plugin.SubjectSelector{Path: "Store.app"}, State: "signed", Signer: "apple:app-store:ABCDE12345", Authority: "Mac App Store"}, []string{"  Signer: Mac App Store\n", "    - signer: apple:app-store:ABCDE12345\n"}},
		{signature.Observation{Input: "vendor", Subject: plugin.SubjectSelector{Path: "Install.app"}, State: "unsigned"}, []string{"Input: vendor", "Subject: Install.app", "Signing state: unsigned", `input: "vendor"`, "unsigned: true"}},
	} {
		evidence, err := json.Marshal([]signature.Observation{test.observation})
		if err != nil {
			t.Fatal(err)
		}
		resource := engine.ResourceReport{Artifacts: map[string]engine.Prepared{"installer": {Evidence: map[string]json.RawMessage{"signatures": evidence}}}}
		got := signatureDetails(resource)
		for _, want := range test.want {
			if !strings.Contains(got, want) {
				t.Fatalf("report lost %q: %s", want, got)
			}
		}
		if status := resourceStatus("signature", resource); status != "signer derived" {
			t.Fatalf("status %q", status)
		}
	}
	built := engine.ResourceReport{Artifacts: map[string]engine.Prepared{"installer": {Filename: "built.pkg"}}}
	if status := resourceStatus("signature", built); status != "nothing to declare" {
		t.Fatalf("status %q for a package without a publisher", status)
	}
}

func TestCancellationWithoutInterruptIsFailure(t *testing.T) {
	var logs bytes.Buffer
	output := newCommandOutput(io.Discard, &logs)
	output.finish(context.Canceled)
	if logs.String() != "Error: context canceled\n" {
		t.Fatal(logs.String())
	}
}

func TestPlanCreationReviewIsOptionalAndDetailsPreserveTheObject(t *testing.T) {
	resource := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Destinations: []engine.DestinationReport{{Name: "repo", Artifact: "alternate.pkg", Changes: []plugin.Change{{Action: "create", Field: "pkginfo", After: json.RawMessage(`{"future_field":{"enabled":false}}`), Review: []string{"Other fields", "  future_field: configured"}}}}}}
	normal := renderResourceDetail(textStyle{}, "plan", resource, false)
	detailed := renderResourceDetail(textStyle{}, "plan", resource, true)
	if !strings.Contains(normal, "alternate.pkg → repo") || !strings.Contains(normal, "future_field: configured") || !strings.Contains(normal, "--details") {
		t.Fatalf("normal review: %s", normal)
	}
	if !strings.Contains(detailed, `"enabled": false`) || strings.Contains(detailed, "configured") {
		t.Fatalf("detailed object: %s", detailed)
	}
	resource.Destinations[0].Changes[0].Review = nil
	if got := renderResourceDetail(textStyle{}, "plan", resource, false); !strings.Contains(got, "future_field:") || !strings.Contains(got, "enabled: false") {
		t.Fatalf("plugin without review lost fields: %s", got)
	}
}

func TestAppliedUploadNamesArtifactInsteadOfDigest(t *testing.T) {
	changes := []plugin.Change{{Action: "upload", Field: "software.icon", Filename: "Example.png", After: json.RawMessage(`"content-digest"`)}}
	if got := strings.Join(appliedActions(changes), "\n"); got != "Uploaded software.icon: Example.png" {
		t.Fatal(got)
	}
}

func TestAppliedRetentionNamesDeletedVersion(t *testing.T) {
	items := []plugin.Change{{Kind: "retention", Action: "delete", Field: "package", Before: json.RawMessage(`"11.4.2"`)}, {Action: "upload", Field: "package.installer", After: json.RawMessage(`"9dc0e5f4abecfe9dc02d45fafc9813881bd24ecb84125eabe522b01b11a039ca"`)}}
	got := strings.Join(appliedActions(items), "\n")
	if got != "Deleted package: 11.4.2 (retention)\nUploaded package.installer" {
		t.Fatal(got)
	}
}

func TestExplicitSelectionAcknowledgesUnchangedResource(t *testing.T) {
	resource := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Key: "stemma/v1alpha1/MacSoftware/example", Destinations: []engine.DestinationReport{{Name: "repo"}}}
	for _, selector := range []string{"", "example", "MacSoftware/example", resource.Key, "MacSoftware/other"} {
		t.Run(selector, func(t *testing.T) {
			var out, logs bytes.Buffer
			output := newCommandOutput(&out, &logs)
			if selector != "" {
				output.selectors = []string{selector}
			}
			if err := output.resourceDone("apply", resource); err != nil {
				t.Fatal(err)
			}
			want := selector != "" && selector != "MacSoftware/other"
			if strings.Contains(out.String(), "No changes") != want {
				t.Fatalf("output: %q", out.String())
			}
		})
	}
}

func TestPublicationVersionsFollowSelectedOutputs(t *testing.T) {
	for _, tt := range []struct {
		name         string
		destinations []engine.DestinationReport
		want, absent []string
	}{
		{"alternate only", []engine.DestinationReport{{Name: "repo", Artifact: "alternate.pkg", Version: "2.0"}}, []string{"➤ MacSoftware/example · 2.0", "alternate.pkg → repo"}, []string{"· 1.0"}},
		{"different outputs", []engine.DestinationReport{{Name: "first", Artifact: "default.pkg", Version: "1.0"}, {Name: "second", Artifact: "alternate.pkg", Version: "2.0"}}, []string{"➤ MacSoftware/example\n", "default.pkg → first · 1.0", "alternate.pkg → second · 2.0"}, []string{"➤ MacSoftware/example ·"}},
		{"unknown version", []engine.DestinationReport{{Name: "repo", Artifact: "alternate.pkg"}}, []string{"➤ MacSoftware/example\n", "alternate.pkg → repo"}, []string{"· 1.0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resource := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Artifacts: map[string]engine.Prepared{"installer": {Version: "1.0"}}, Destinations: tt.destinations}
			got := renderResourceDetail(textStyle{}, "plan", resource, false)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q: %s", want, got)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(got, absent) {
					t.Errorf("unexpected %q: %s", absent, got)
				}
			}
		})
	}
}

func TestResourceNoticesSurviveResultFiltering(t *testing.T) {
	resource := engine.ResourceReport{Kind: "MacSoftware", Name: "foo", Cached: true, Destinations: []engine.DestinationReport{{Name: "repo"}}, Notices: []plugin.Notice{
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
			if out.Len() != 0 {
				t.Fatalf("unchanged resource streamed: %s", &out)
			}
			// A later reconcile phase must not repeat the same recommendation.
			if err := output.resourceDone("plan", resource); err != nil {
				t.Fatal(err)
			}
			report := engine.Report{Resources: []engine.ResourceReport{resource}}
			report.Summarize("plan")
			if err := output.report(&out, "plan", report, nil); err != nil {
				t.Fatal(err)
			}
			output.finish(nil)
			if asJSON {
				var decoded engine.Report
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				if diagnostic.Len() != 0 || len(decoded.Resources[0].Notices) != 2 {
					t.Fatalf("report=%s stderr=%s", &out, &diagnostic)
				}
			} else if want := "! MacSoftware/foo · Source has no signature expectation\n  Run `stemma signature MacSoftware/foo` to derive one.\n" +
				"! MacSoftware/foo · Developer ID signature has no secure timestamp; certificate validity at signing time cannot be established\n"; diagnostic.String() != want {
				t.Fatalf("stderr=%q want=%q", diagnostic.String(), want)
			}
		})
	}
}
