package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/plugin"
)

func TestNestedStagesReportOnTheirOperationRow(t *testing.T) {
	var rows, logs bytes.Buffer
	o := &commandOutput{out: &rows, interactive: true}
	logger := slog.New(&stageHandler{Handler: slog.NewTextHandler(&logs, nil), output: o})
	ctx := plugin.WithLogger(t.Context(), logger.With("resource", "MacSoftware/example"))
	acquired := plugin.Stage(ctx, "Acquiring input")
	downloaded := plugin.Stage(ctx, "Downloading input")
	for current := range 8 {
		plugin.Logger(ctx).Info("Transfer progress", "progress", true, "current", current, "total", 7, "unit", "bytes")
	}
	live := progressModel{groups: o.progress.snapshot(), width: 100, height: 20, spinner: spinner.New()}.View().Content
	heading, row, _ := strings.Cut(live, "\n")
	if heading != "MacSoftware/example" || strings.Contains(row, "\n") || !strings.Contains(row, "Downloading input  7 B / 7 B 100% (0s)") {
		t.Fatalf("nested stage did not report on its operation row: %s", live)
	}
	downloaded(nil)
	acquired(nil, "cached", false)
	prepared := plugin.Stage(ctx, "Preparing outputs")
	plugin.Stage(ctx, "Inspecting application")(nil)
	verified := plugin.Stage(ctx, "Verifying installer")
	verified(errors.New("invalid signature"))
	prepared(errors.New("invalid signature"))
	plugin.Logger(ctx).Info("Provider notice")
	if err := o.resourceDone(&bytes.Buffer{}, true, "prepare", engine.ResourceReport{Kind: "MacSoftware", Name: "example", Error: "invalid signature"}); err != nil {
		t.Fatal(err)
	}
	o.endProgress(errors.New("an earlier resource failed"))
	text := rows.String()
	for _, want := range []string{"✗ MacSoftware/example  failed", "    ✓ Acquiring input  7 B / 7 B 100%", "    ✗ Preparing outputs\n      invalid signature"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for _, step := range []string{"Downloading input", "Inspecting application", "Verifying installer"} {
		if strings.Contains(text, step) {
			t.Fatalf("step %q became a separate row: %s", step, text)
		}
	}
	if strings.Count(text, "invalid signature") != 1 || strings.Count(text, "MacSoftware/example") != 1 || strings.Contains(text, "(0s)") {
		t.Fatalf("duplicate or noisy rows: %s", text)
	}
	if strings.Contains(logs.String(), "Transfer progress") || !strings.Contains(logs.String(), "Provider notice") {
		t.Fatalf("diagnostics: %s", logs.String())
	}
}

func TestIdleResourcesLeaveTheLiveRegion(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	p.update(activity{scope: "first", label: "Acquiring input", stage: true})
	p.update(activity{scope: "first", label: "Acquiring input", status: true, elapsed: 2 * time.Second})
	p.update(activity{scope: "second", label: "Acquiring input", stage: true})
	if live := p.snapshot(); len(live) != 1 || live[0][0].label != "second" {
		t.Fatalf("idle resource held the live region: %+v", live)
	}
	p.update(activity{scope: "second", label: "Acquiring input", status: true})
	if live := p.snapshot(); len(live) != 1 || live[0][0].label != "second" {
		t.Fatalf("gap between stages blanked the live region: %+v", live)
	}
	p.update(activity{scope: "first", label: "Planning destination", stage: true})
	p.update(activity{scope: "first", label: "Planning destination", status: true, elapsed: time.Second})
	p.complete("first", "unchanged", false, nil)
	p.stop("")
	if !strings.Contains(out.String(), "✓ first  unchanged (3s)\n") {
		t.Fatalf("heading did not report working time: %s", out.String())
	}
}

func TestInterleavedResourcesKeepSeparateTrees(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	p.update(activity{scope: "first", label: "Download", stage: true})
	p.update(activity{scope: "second", label: "Download", stage: true})
	p.update(activity{scope: "first", label: "Download", status: true})
	p.complete("first", "prepared", false, nil)
	p.stop("interrupted")
	text := out.String()
	if strings.Count(text, "first") != 1 || strings.Count(text, "second") != 1 || !strings.Contains(text, "✓ first  prepared") || !strings.Contains(text, "✗ second  interrupted") || strings.Contains(text, "    ✓ Download") {
		t.Fatalf("resource results: %s", text)
	}
}

func TestUnknownProgressDoesNotInventPercentage(t *testing.T) {
	var out bytes.Buffer
	o := &commandOutput{out: &out, interactive: true}
	o.progress = newTerminalProgress(&out)
	o.progress.update(activity{scope: "example", label: "Downloading input", stage: true})
	o.progress.update(activity{scope: "example", current: 10, total: -1, unit: "bytes", progress: true})
	o.endProgress(context.Canceled)
	if strings.Contains(out.String(), "%") || !strings.Contains(out.String(), "10 B") || !strings.Contains(out.String(), "interrupted") || strings.Contains(out.String(), "✓") {
		t.Fatalf("unknown progress: %s", out.String())
	}
}

func TestProgressLogVolumeRespectsVerbosity(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		var out bytes.Buffer
		o := &commandOutput{out: &out}
		logger := slog.New(&stageHandler{Handler: slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: level}), output: o})
		for current := range 10 {
			logger.Info("Transfer progress", "progress", true, "current", current, "total", 9, "unit", "bytes", "progress_final", current == 9)
		}
		want := 1
		if level == slog.LevelDebug {
			want = 10
		}
		if got := strings.Count(out.String(), "Transfer progress"); got != want {
			t.Fatalf("level %s: %d logs, want %d", level, got, want)
		}
	}
}

func TestSuccessfulResourceCollapsesButRetainsPlanChanges(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	p.update(activity{scope: "example", label: "Planning destination", stage: true})
	p.update(activity{scope: "example", label: "Planning destination", status: true})
	p.note("example", "repo: set description", "detail")
	p.complete("example", "1 planned change", false, nil)
	p.stop("")
	text := out.String()
	if !strings.Contains(text, "✓ example  1 planned change") || !strings.Contains(text, "    - repo: set description") || strings.Contains(text, "Planning destination") {
		t.Fatalf("completed plan: %s", text)
	}
}

func TestProjectSetupDoesNotBecomeAResourceResult(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	p.update(activity{label: "Loading project", stage: true})
	p.update(activity{label: "Loading project", status: true})
	p.update(activity{scope: "example", label: "Verify installer", stage: true})
	p.stop("interrupted")
	if strings.Contains(out.String(), "Project") || !strings.Contains(out.String(), "example  interrupted") {
		t.Fatalf("project result: %s", out.String())
	}
}

func TestActiveTreesKeepHeadingsWhenResized(t *testing.T) {
	now := time.Now()
	model := progressModel{width: 80, height: 20, spinner: spinner.New()}
	groups := progressSnapshot{
		{
			{label: "MacSoftware/first", heading: true, started: now},
			{label: "Acquiring input", started: now, ended: now, outcome: "done"},
			{label: "Verifying installer", started: now},
		},
		{
			{label: "MacSoftware/second", heading: true, started: now},
			{label: "Acquiring input", started: now, ended: now, outcome: "done"},
			{label: "Inspecting application", started: now},
		},
	}
	updated, _ := model.Update(groups)
	full := updated.View().Content
	if strings.Count(full, "Acquiring input") != 2 || strings.Index(full, "MacSoftware/first") > strings.Index(full, "MacSoftware/second") {
		t.Fatalf("active trees out of order: %s", full)
	}
	resized, _ := updated.Update(tea.WindowSizeMsg{Width: 60, Height: 5})
	short := resized.View().Content
	for _, want := range []string{"MacSoftware/first", "Verifying installer", "MacSoftware/second", "Inspecting application"} {
		if strings.Count(short, want) != 1 {
			t.Fatalf("missing or repeated active row %q: %s", want, short)
		}
	}
	if strings.Contains(short, "Acquiring input") || strings.Count(short, "\n") != 3 {
		t.Fatalf("short terminal loses the active work: %s", short)
	}
}
