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
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/engine"
	pluginstore "github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/reconcile"
	"github.com/woodleighschool/stemma/internal/source"
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
	for _, flag := range []string{"--log-level", "--log-format", "--quiet", "--verbose", "--debug", "--no-progress"} {
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
  signature: {signer: apple:developer-id:SMLKBTR495}
`)
	var out, logs bytes.Buffer
	cmd, finish := command(&out, &logs)
	cmd.SetArgs([]string{"prepare", "--root", project, "--cache-dir", t.TempDir(), "--json"})
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
	if logs.Len() != 0 {
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
	project := t.TempDir()
	testproject.Write(t, filepath.Join(project, "stemma.yaml"), `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: fixture}
spec:
  plugins:
    echo: {trusted: true, path: plugins/echo, entrypoint: echo}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: example}
spec:
  source: {path: example.pkg}
  signature: {signer: apple:developer-id:SMLKBTR495}
`)
	if got := run("plugins", "list", "--root", project); got != "echo: plugins/echo (echo)\n" {
		t.Fatalf("plugins list: %q", got)
	}
}

func TestLockedPluginsNameWhatChanged(t *testing.T) {
	local := func(digest string) pluginstore.Entry {
		return pluginstore.Entry{Path: "plugins/echo", Local: &source.Entry{Content: source.Content{Artifact: cas.Ref{SHA256: digest}}}}
	}
	previous := map[string]pluginstore.Entry{"echo": local(strings.Repeat("a", 64)), "kept": {Image: "registry.example/kept:1", Digest: "sha256:" + strings.Repeat("c", 64)}}
	locked := map[string]pluginstore.Entry{"echo": local(strings.Repeat("b", 64)), "kept": previous["kept"], "new": {Image: "registry.example/new:1", Digest: "sha256:" + strings.Repeat("d", 64)}}
	var out bytes.Buffer
	if err := printLockedPlugins(&out, previous, locked, true); err != nil {
		t.Fatal(err)
	}
	want := "echo: plugins/echo sha256:bbbbbbbbbbbb updated\nkept: registry.example/kept:1 sha256:cccccccccccc unchanged\nnew: registry.example/new:1 sha256:dddddddddddd locked\nLockfile updated.\n"
	if out.String() != want {
		t.Fatalf("locked plugins:\n%s", out.String())
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
		for _, want := range []string{"MacSoftware/changed: 1 planned change\n  repo: 1 planned change\n    package.version: 1 -> 2\n", "MacSoftware/broken: failed\n  error: invalid signature\n"} {
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
		if got := strings.TrimPrefix(human.String(), streamed); got != "Plan: 1 resource with changes across 1 destination, 1 unchanged, 1 failed.\n" {
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
		want := 2
		if all {
			want = 3
		}
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
	text := renderResource(textStyle{}, "apply", report.Resources[0]) + renderSummary(textStyle{}, "apply", report, errors.New("upload failed"))
	for _, want := range []string{"MacSoftware/example: failed\n", "  first: 1 change applied\n    description: old -> new\n", "  second: failed (changes not confirmed)\n", "    error: upload failed\n", "Apply incomplete: 1 destination applied", "1 failed"} {
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
	if err := o.report(&out, "apply", report, context.Canceled); err != nil {
		t.Fatal(err)
	}
	o.finish(context.Canceled)
	if !strings.HasPrefix(out.String(), "MacSoftware/done: 1 change applied\n") || !strings.HasSuffix(out.String(), "Apply interrupted: 1 destination applied, 0 resources unchanged.\n") || logs.String() != "Interrupted.\n" {
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
		{resource, ""},
		{reconcile.ErrFailed, ""},
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
	o.pathOnly = true
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
  signature: {signer: apple:developer-id:SMLKBTR495}
`)
	var stderr bytes.Buffer
	cmd, finish := command(failingReportWriter{}, &stderr)
	cmd.SetArgs([]string{"prepare", "--root", project, "--cache-dir", t.TempDir(), "--json"})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	for _, want := range []string{"output unavailable", "missing-installer", "missing.pkg"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("unpublished error lost %q: %s", want, stderr.String())
		}
	}
}
