package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/testproject"
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
		{"--log-level", "trace"}, {"--log-format", "yaml"}, {"--output", "yaml"},
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
	if err := testproject.Write(filepath.Join(project, "stemma.yaml"), []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: fixture}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: missing-installer}
spec:
  source: {path: missing.pkg}
  verification: {integrity: true}
`)); err != nil {
		t.Fatal(err)
	}
	var out, logs bytes.Buffer
	cmd, finish := command(&out, &logs)
	cmd.SetArgs([]string{"prepare", "--root", project, "--cache-dir", t.TempDir(), "--output", "json", "--log-format", "json"})
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
	if err := output.resourceDone(io.Discard, "text", "prepare", engine.ResourceReport{Kind: "MacSoftware", Name: "example"}); err != nil {
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
