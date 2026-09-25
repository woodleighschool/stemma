package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/plugin"
)

func TestStepsShowUnderTheirStageUntilItFinishes(t *testing.T) {
	var out, logs bytes.Buffer
	o := newCommandOutput(&out, &logs)
	o.interactive = true
	ctx := plugin.WithLogger(t.Context(), slog.New(&activityHandler{output: o}).With("resource", "MacSoftware/example"))
	input := plugin.WithLogger(ctx, plugin.Logger(ctx).With("input", "source"))
	acquired := plugin.Stage(input, "Acquiring input")
	downloaded := plugin.Stage(input, "Downloading input", plugin.Detail("example.pkg"))
	for current := range 8 {
		plugin.Logger(input).Info("Transfer progress", "progress", true, "current", current, "total", 7, "unit", "bytes")
	}
	live := strings.Split(progressModel{groups: o.progress.snapshot(), width: 100, height: 20, spinner: spinner.New()}.View().Content, "\n")
	if len(live) != 3 || live[0] != "MacSoftware/example" || !strings.HasPrefix(live[1], "    ") || !strings.Contains(live[1], "Acquiring input (source)  (0s)") ||
		!strings.HasPrefix(live[2], "      ") || !strings.Contains(live[2], "Downloading input  example.pkg  "+strings.Repeat("━", barWidth)+"  7 B / 7 B  (0s)") {
		t.Fatalf("step is not nested under its stage: %q", live)
	}
	downloaded(nil)
	acquired(nil, plugin.Detail("example.pkg (cached)"))
	plugin.Stage(ctx, "Verifying installer")(errors.New("invalid signature"))
	live = strings.Split(progressModel{groups: o.progress.snapshot(), width: 100, height: 20, spinner: spinner.New()}.View().Content, "\n")
	if len(live) != 3 || !strings.Contains(live[1], "✓ Acquiring input (source)  example.pkg (cached)") || !strings.Contains(live[2], "✗ Verifying installer") {
		t.Fatalf("finished stages did not fold their steps: %q", live)
	}
	if err := o.resourceDone("prepare", engine.ResourceReport{Kind: "MacSoftware", Name: "example", Error: "invalid signature"}); err != nil {
		t.Fatal(err)
	}
	if len(o.progress.snapshot()) != 0 || out.String() != "MacSoftware/example: failed\n  error: invalid signature\n\n" || logs.Len() != 0 {
		t.Fatalf("finished resource: live=%v stdout=%q stderr=%q", o.progress.snapshot(), out.String(), logs.String())
	}
}

func TestFinishedResourcesLeaveAnOutcomeLineInATerminal(t *testing.T) {
	unchanged := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Destinations: []engine.DestinationReport{{Name: "munki"}}}
	for _, interactive := range []bool{true, false} {
		var out bytes.Buffer
		o := newCommandOutput(&out, io.Discard)
		o.interactive = interactive
		if err := o.resourceDone("plan", unchanged); err != nil {
			t.Fatal(err)
		}
		want := ""
		if interactive {
			want = "MacSoftware/example: unchanged\n"
		}
		if out.String() != want {
			t.Fatalf("interactive=%v: %q, want %q", interactive, out.String(), want)
		}
	}
}

func TestNarrowRowsDropTheBarBeforeTheirSubject(t *testing.T) {
	now := time.Now()
	line := progressLine{label: "Downloading input", detail: "Example Application Installer.pkg", unit: "bytes", current: 1 << 20, total: 4 << 20, depth: 1, started: now}
	wide := progressText(textStyle{}, &line, 120, now, "|")
	if !strings.Contains(wide, "Example Application Installer.pkg  "+strings.Repeat("━", 5)+strings.Repeat("─", 15)+"  1.0 MiB / 4.0 MiB  (0s)") {
		t.Fatalf("wide row: %q", wide)
	}
	narrow := progressText(textStyle{}, &line, 90, now, "|")
	if strings.Contains(narrow, "━") || !strings.Contains(narrow, "Downloading input  Example Application Installer.pkg  1.0 MiB / 4.0 MiB  (0s)") {
		t.Fatalf("narrow row: %q", narrow)
	}
	if narrower := progressText(textStyle{}, &line, 70, now, "|"); !strings.Contains(narrower, "Downloading input  Example Applicatio…  1.0 MiB / 4.0 MiB  (0s)") {
		t.Fatalf("narrower row: %q", narrower)
	}
	for _, width := range []int{10, 30, 50} {
		if text := progressText(textStyle{}, &line, width, now, "|"); runewidth.StringWidth(text) >= width {
			t.Fatalf("row wider than the terminal at %d: %q", width, text)
		}
	}
}

func TestIdleResourcesLeaveTheLiveRegion(t *testing.T) {
	p := newTerminalProgress(io.Discard)
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
	p.complete("second")
	p.update(activity{scope: "first", label: "Planning destination", stage: true})
	if live := p.snapshot(); len(live) != 1 || live[0][0].label != "first" || len(live[0]) != 3 {
		t.Fatalf("resumed resource lost its tree: %+v", live)
	}
}

func TestProjectSetupLeavesTheTreeUnlessItFails(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{label: "Loading project", stage: true})
	p.update(activity{label: "Loading project", status: true})
	if p.find("") != nil {
		t.Fatal("successful project setup stayed in the tree")
	}
	p.update(activity{label: "Locking plugins", stage: true})
	p.update(activity{label: "Locking plugins", status: true, err: "registry unavailable"})
	if group := p.find(""); group == nil || group.rows[0].label != "Project" || group.rows[1].outcome != "failed" {
		t.Fatalf("failed project stage left the tree: %+v", group)
	}
}

func TestUnknownProgressDoesNotInventPercentage(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{scope: "example", label: "Downloading input", stage: true})
	p.update(activity{scope: "example", current: 10, total: -1, unit: "bytes", progress: true})
	live := progressModel{groups: p.snapshot(), width: 100, height: 20, spinner: spinner.New()}.View().Content
	if strings.Contains(live, "%") || !strings.Contains(live, "10 B") {
		t.Fatalf("unknown progress: %s", live)
	}
	stopped, _ := progressModel{groups: p.snapshot()}.Update(stopProgress{})
	if stopped.View().Content != "" {
		t.Fatalf("stopped progress retained: %s", stopped.View().Content)
	}
}

func TestProgressCountsNameTheirUnitOnce(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		line progressLine
		want string
	}{
		{progressLine{label: "Extracting files", unit: "entries", current: 1234, total: 5678, started: now}, "1,234 / 5,678 entries  (0s)"},
		{progressLine{label: "Extracting files", unit: "entries", current: 5678, started: now, ended: now, outcome: "done"}, "✓ Extracting files  5,678 entries"},
	} {
		if text := progressText(textStyle{}, &test.line, 100, now, "|"); !strings.HasSuffix(text, test.want) {
			t.Fatalf("%q does not end with %q", text, test.want)
		}
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

func TestOnlyStageStartsTheLiveTree(t *testing.T) {
	o := newCommandOutput(io.Discard, io.Discard)
	o.interactive = true
	logger := slog.New(&activityHandler{output: o})
	logger.Info("Transfer progress", "progress", true, "current", 1)
	if o.progress != nil {
		t.Fatal("progress without a stage opened the live tree")
	}
	plugin.Stage(plugin.WithLogger(t.Context(), logger), "Acquiring input")(nil)
	if o.progress == nil {
		t.Fatal("stage did not open the live tree")
	}
}

func TestLiveTreeEligibility(t *testing.T) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skip("requires controlling terminal")
	}
	defer func() { _ = tty.Close() }()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	for _, test := range []struct {
		name           string
		stdout, stderr io.Writer
		want           bool
	}{
		{"interactive", tty, tty, true},
		{"pipe stdout", io.Discard, tty, false},
		{"pipe stderr", tty, io.Discard, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "plan"}
			cmd.SetContext(t.Context())
			output := newCommandOutput(test.stdout, test.stderr)
			if err := output.start(cmd); err != nil {
				t.Fatal(err)
			}
			if output.interactive != test.want {
				t.Fatalf("interactive=%v want %v", output.interactive, test.want)
			}
		})
	}
}

func TestFinalWriteEndsTheLiveTreePermanently(t *testing.T) {
	var text bytes.Buffer
	o := newCommandOutput(&text, io.Discard)
	o.interactive = true
	o.progress = newTerminalProgress(io.Discard)
	writer := finalWriter{Writer: &text, output: o}
	if _, err := io.WriteString(writer, "report\n"); err != nil {
		t.Fatal(err)
	}
	plugin.Stage(plugin.WithLogger(t.Context(), slog.New(&activityHandler{output: o})), "Cleanup")(nil)
	if text.String() != "report\n" || o.progress != nil || o.interactive {
		t.Fatalf("final report reopened the live tree: %q", text.String())
	}
}
