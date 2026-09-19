package main

import (
	"bytes"
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
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestOutputLevelsAndReports(t *testing.T) {
	for _, test := range []struct {
		name                 string
		flags                []string
		info, debug, warning bool
	}{
		{name: "default", info: true, warning: true},
		{name: "verbose", flags: []string{"-v"}, info: true, debug: true, warning: true},
		{name: "debug", flags: []string{"--debug"}, info: true, debug: true, warning: true},
		{name: "explicit debug", flags: []string{"--log-level", "debug"}, info: true, debug: true, warning: true},
		{name: "quiet", flags: []string{"-q"}, warning: true},
		{name: "errors", flags: []string{"--log-level", "error"}},
		{name: "plain", flags: []string{"--no-progress"}, info: true, warning: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, logs bytes.Buffer
			cmd, finish := command(&out, &logs)
			cmd.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
				plugin.Stage(cmd.Context(), "Acquiring input")
				plugin.Logger(cmd.Context()).DebugContext(cmd.Context(), "Cache lookup")
				plugin.Logger(cmd.Context()).WarnContext(cmd.Context(), "Verification disabled")
				_, err := io.WriteString(cmd.OutOrStdout(), "report\n")
				return err
			}})
			cmd.SetArgs(append(test.flags, "probe"))
			err := cmd.ExecuteContext(t.Context())
			finish(err)
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != "report\n" {
				t.Fatalf("logs contaminated stdout: %q", out.String())
			}
			for message, want := range map[string]bool{"Acquiring input": test.info, "Cache lookup": test.debug, "Verification disabled": test.warning} {
				if strings.Contains(logs.String(), message) != want {
					t.Errorf("%q enabled=%t: %s", message, want, logs.String())
				}
			}
			if strings.ContainsAny(logs.String(), "\x1b\r") {
				t.Fatalf("non-terminal received control codes: %q", logs.String())
			}
		})
	}
}

func TestInvalidOutputOptionsDoNotRun(t *testing.T) {
	for _, flags := range [][]string{
		{"--log-level", "trace"}, {"--log-format", "yaml"},
		{"--quiet", "--verbose"}, {"--log-level", "info", "--debug"},
	} {
		var out, logs bytes.Buffer
		cmd, finish := command(&out, &logs)
		called := false
		cmd.AddCommand(&cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { called = true; return nil }})
		cmd.SetArgs(append(flags, "probe"))
		err := cmd.ExecuteContext(t.Context())
		finish(err)
		if err == nil || called || out.Len() != 0 {
			t.Fatalf("flags=%v error=%v called=%t stdout=%q", flags, err, called, out.String())
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
	cmd.SetArgs([]string{"prepare", "--root", project, "--cache-dir", t.TempDir(), "--json", "--log-format", "json"})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	if err == nil {
		t.Fatal("missing input succeeded")
	}
	var report engine.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.LockChanged != nil || report.Error == "" {
		t.Fatalf("failure reported an unchecked lockfile outcome: %+v", report)
	}
	decoder := json.NewDecoder(&logs)
	var messages []string
	for {
		var record struct {
			Message string `json:"msg"`
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, record.Message)
	}
	if !strings.Contains(strings.Join(messages, "\n"), "Acquiring input") || messages[len(messages)-1] != "Command failed" {
		t.Fatalf("missing stage or failure log: %v (error: %s)", messages, report.Error)
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

func TestTextLogsNameEachStageOnce(t *testing.T) {
	for _, test := range []struct {
		name, format string
		level        slog.Level
		want         int
	}{
		{name: "text", format: "text", level: slog.LevelInfo, want: 1},
		{name: "debug text", format: "text", level: slog.LevelDebug, want: 2},
		{name: "json", format: "json", level: slog.LevelInfo, want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			output := &commandOutput{out: io.Discard, format: test.format}
			logger := slog.New(&stageHandler{Handler: slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: test.level}), output: output})
			ctx := plugin.WithLogger(t.Context(), logger)
			plugin.Stage(ctx, "Acquiring input")(nil)
			plugin.Stage(ctx, "Preparing outputs")(errors.New("invalid signature"))
			for _, stage := range []string{"Acquiring input", "Preparing outputs"} {
				if got := strings.Count(logs.String(), stage); got != test.want {
					t.Fatalf("%q logged %d times, want %d: %s", stage, got, test.want, logs.String())
				}
			}
		})
	}
}

func TestFinalErrorIsRenderedOnceForPeople(t *testing.T) {
	failure := errors.Join(
		engine.ReportedError{Err: errors.New("MacSoftware/example: invalid signature")},
		errors.New("lockfile: schema violations:\n\tspec.source: required\n\tspec.icon: unknown"),
	)
	var logs bytes.Buffer
	output := &commandOutput{out: &logs, format: "text", failed: 1}
	output.finish(failure)
	want := "Error: lockfile: schema violations:\n    spec.source: required\n    spec.icon: unknown\nError: 1 resource failed\n"
	if logs.String() != want {
		t.Fatalf("final error:\n%s", logs.String())
	}
	logs.Reset()
	output = &commandOutput{out: &logs, format: "json"}
	output.finish(failure)
	var record struct {
		Message string `json:"msg"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil || record.Message != "Command failed" || record.Error != failure.Error() {
		t.Fatalf("JSON logs lost the raw error: %s (%v)", logs.String(), err)
	}
}

func TestFinalErrorKeepsNestedFailuresAndContext(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "joined failures",
			err: errors.Join(errors.Join(
				engine.ReportedError{Err: errors.New("upload failed")},
				errors.New("report write failed"),
			), errors.New("lock write failed")),
			want: "Error: report write failed\nError: lock write failed\nError: 1 resource failed\n",
		},
		{
			name: "wrapped failures",
			err:  fmt.Errorf("project: %w", errors.Join(errors.New("source missing"), errors.New("icon missing"))),
			want: "Error: project: source missing\n  icon missing\nError: 1 resource failed\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := &commandOutput{failed: 1}
			if got := output.failure(test.err); got != test.want {
				t.Fatalf("final error = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOnlyAStageOpensTheLiveTree(t *testing.T) {
	var logs bytes.Buffer
	output := &commandOutput{out: io.Discard, interactive: true}
	logger := slog.New(&stageHandler{Handler: slog.NewTextHandler(&logs, nil), output: output})
	ctx := plugin.WithLogger(t.Context(), logger.With("resource", "MacSoftware/example"))
	plugin.Logger(ctx).WarnContext(ctx, "Verification disabled")
	plugin.Logger(ctx).InfoContext(ctx, "Transfer progress", "progress", true, "current", 1, "unit", "bytes", "progress_final", true)
	if output.progress != nil || !strings.Contains(logs.String(), "Verification disabled") {
		t.Fatalf("a record without a stage opened the live tree: %s", logs.String())
	}
	plugin.Stage(ctx, "Acquiring input")
	if output.progress == nil {
		t.Fatal("a stage did not open the live tree")
	}
	output.stop()
}

func TestInteractiveStagesDoNotBecomePermanentLogs(t *testing.T) {
	var logs bytes.Buffer
	output := &commandOutput{out: io.Discard, interactive: true}
	logger := slog.New(&stageHandler{Handler: slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}), output: output})
	ctx := plugin.WithLogger(t.Context(), logger.With("resource", "MacSoftware/example"))
	plugin.Stage(ctx, "Inspecting application")
	plugin.Stage(ctx, "Verifying installer")
	plugin.Logger(ctx).InfoContext(ctx, "Resource prepared", "stage_result", true, "cached", false)
	plugin.Logger(ctx).InfoContext(ctx, "Provider notice")
	plugin.Logger(ctx).DebugContext(ctx, "Cache lookup")
	plugin.Logger(ctx).WarnContext(ctx, "Verification disabled")
	if err := output.resourceDone(io.Discard, false, "prepare", engine.ResourceReport{Kind: "MacSoftware", Name: "example"}); err != nil {
		t.Fatal(err)
	}
	output.stop()
	for _, unwanted := range []string{"Inspecting application", "Verifying installer", "Resource prepared"} {
		if strings.Contains(logs.String(), unwanted) {
			t.Fatalf("live activity leaked into permanent logs: %s", logs.String())
		}
	}
	for _, want := range []string{"Cache lookup", "Provider notice"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("diagnostic missing: %s", logs.String())
		}
	}
}

func TestPreparationReportIsASummary(t *testing.T) {
	report := engine.Report{LockChanged: new(false), Resources: []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "example", Artifacts: map[string]engine.Prepared{"installer": {Filename: "example.pkg", Version: "1.0"}}},
		{Kind: "MacSoftware", Name: "cached", Cached: true},
	}}
	var out bytes.Buffer
	if err := printSummary(&out, "prepare", report); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "Preparation: 1 prepared, 1 cached, 0 failed.\nLockfile unchanged.\n"; got != want {
		t.Fatalf("prepare repeated resource details: %q", got)
	}
	out.Reset()
	if err := writeJSON(&out, report); err != nil {
		t.Fatal(err)
	}
	var decoded engine.Report
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded.Resources[0].Artifacts["installer"].Filename != "example.pkg" {
		t.Fatalf("JSON lost artifact details: %s (%v)", out.String(), err)
	}
}

func TestApplyReportCountsOnlySuccessfulDestinations(t *testing.T) {
	report := engine.Report{Resources: []engine.ResourceReport{{Kind: "MacSoftware", Name: "example", Error: "one destination failed", Destinations: []engine.DestinationReport{
		{Name: "first", Applied: true, Changes: []plugin.Change{{Action: "update", Field: "description"}}},
		{Name: "second", Error: "upload failed", Changes: []plugin.Change{{Action: "create", Field: "installer"}}},
	}}}}
	var out bytes.Buffer
	if err := printResource(&out, "apply", report.Resources[0]); err != nil {
		t.Fatal(err)
	}
	if err := printSummary(&out, "apply", report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first: 1 changes applied", "update description", "second: failed: upload failed", "Apply: 1 changes applied, 1 failed resources."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
}
