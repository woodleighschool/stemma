package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/woodleighschool/stemma/internal/engine"
)

// shown returns the live region in a roomy terminal.
func shown(p *terminalProgress, now time.Time) string {
	_, region := p.view(now, 100, 28)
	return region
}

func TestResourceBlockStaysWhileItsStepsChange(t *testing.T) {
	const resource = "MacSoftware/example"
	p := newTerminalProgress(io.Discard)
	now := time.Now()
	p.update(activity{scope: resource, label: "Acquiring input", stage: true})
	p.update(activity{scope: resource, label: "Downloading input", detail: "example.pkg", stage: true})
	p.update(activity{scope: resource, progress: true, current: 1 << 20, total: 4 << 20, unit: "bytes"})
	got := shown(p, now)
	for _, want := range []string{"➤ MacSoftware/example\n", "Downloading input", "example.pkg", "1.0 MiB / 4.0 MiB"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Acquiring") {
		t.Fatalf("enclosing step shown beside its step: %s", got)
	}
	p.update(activity{scope: resource, label: "Downloading input", status: true})
	if got := shown(p, now); !strings.Contains(got, "Acquiring input") || !strings.Contains(got, "✓ Downloading input: example.pkg 1.0 MiB") {
		t.Fatalf("transfer lost its completed count: %s", got)
	}
	p.update(activity{scope: resource, label: "Acquiring input", status: true})
	if got := shown(p, now); got != "➤ MacSoftware/example\n  ✓ Downloading input: example.pkg 1.0 MiB" {
		t.Fatalf("block changed between steps: %q", got)
	}
	p.update(activity{scope: resource, label: "Preparing outputs", stage: true})
	if got := shown(p, now); !strings.HasPrefix(got, "➤ MacSoftware/example\n") || !strings.Contains(got, "Preparing outputs") {
		t.Fatalf("new step waited: %q", got)
	}
	p.complete(resource)
	if got := shown(p, now); got != "" {
		t.Fatalf("completed resource retained: %s", got)
	}
}

func TestSlowStepsStayListedUntilTheResourceCompletes(t *testing.T) {
	const resource = "MacSoftware/example"
	p := newTerminalProgress(io.Discard)
	step := func(label string, elapsed time.Duration, failure string) {
		p.update(activity{scope: resource, label: label, stage: true})
		p.update(activity{scope: resource, label: label, status: true, elapsed: elapsed, err: failure})
	}
	p.update(activity{scope: resource, label: "Discovering release", detail: "downloads.example", stage: true})
	p.update(activity{scope: resource, label: "Discovering release", detail: "1.2.3", status: true, elapsed: 2 * time.Second})
	p.update(activity{scope: resource, label: "Applying destination", stage: true})
	p.update(activity{scope: resource, label: "Uploading installer", detail: "example.pkg", stage: true})
	p.update(activity{scope: resource, progress: true, current: 4 << 20, total: 4 << 20, unit: "bytes"})
	p.update(activity{scope: resource, label: "Uploading installer", status: true, elapsed: 41 * time.Second})
	step("Saving package", 20*time.Millisecond, "")
	step("Verifying publication", 3*time.Second, "not found")
	step("Publishing icon", 20*time.Millisecond, "rejected")
	p.update(activity{scope: resource, label: "Applying destination", status: true, elapsed: time.Minute})
	p.update(activity{scope: resource, label: "Recording artifacts", stage: true})
	want := strings.Join([]string{
		"➤ MacSoftware/example",
		"  ✓ Discovering release: downloads.example (1.2.3) (2s)",
		"  ✓ Uploading installer: example.pkg 4.0 MiB (41s)",
		"  ✗ Verifying publication (3s)",
		"  ✗ Publishing icon",
		"  ⠋ Recording artifacts",
	}, "\n")
	if got := shown(p, time.UnixMilli(0)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	p.update(activity{scope: "MacSoftware/next", label: "Checking preparation cache", stage: true})
	if got := shown(p, time.UnixMilli(0)); !strings.Contains(got, "Recording artifacts") {
		t.Fatalf("running resource gave way to another: %s", got)
	}
	p.update(activity{scope: resource, label: "Recording artifacts", status: true})
	p.update(activity{scope: "MacSoftware/next", label: "Materializing inputs", stage: true})
	if got := shown(p, time.UnixMilli(0)); strings.Contains(got, resource) {
		t.Fatalf("idle resource outlived the next one's work: %s", got)
	}
}

func TestPersistentLinesLeaveTheLiveRegionOnScreen(t *testing.T) {
	var stdout, stderr bytes.Buffer
	p := newTerminalProgress(&stderr)
	p.update(activity{scope: "MacSoftware/example", label: "Uploading installer", stage: true})
	p.draw(time.Now())
	if err := p.write(&stdout, "a warning\n"); err != nil {
		t.Fatal(err)
	}
	_, after, erased := strings.Cut(stderr.String(), "\x1b[J")
	if stdout.String() != "a warning\n" || !erased || !strings.Contains(after, "➤ MacSoftware/example\n") || !strings.Contains(after, "Uploading installer") {
		t.Fatalf("region not restored after the line: %q", stderr.String())
	}
}

func TestPersistentResultsClearProgressAndKeepTheirStream(t *testing.T) {
	var stdout, stderr bytes.Buffer
	p := newTerminalProgress(&stderr)
	p.update(activity{scope: "MacSoftware/example", label: "Preparing", stage: true})
	p.draw(time.Now().Add(time.Second))
	p.complete("MacSoftware/example")
	report := strings.Repeat("a long report line\n", 80)
	if err := p.write(&stdout, report); err != nil {
		t.Fatal(err)
	}
	p.draw(time.Now().Add(2 * time.Second))
	p.stop()
	if stdout.String() != report {
		t.Fatalf("stdout lost report: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "a long report") || !strings.HasSuffix(stderr.String(), "\x1b[J") {
		t.Fatalf("report mixed with progress: %q", stderr.String())
	}
}

func TestHumanSelectionDoesNotDependOnTerminal(t *testing.T) {
	unchanged := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Destinations: []engine.DestinationReport{{Name: "repo"}}}
	for _, interactive := range []bool{true, false} {
		var out bytes.Buffer
		o := newCommandOutput(&out, io.Discard)
		o.interactive = interactive
		if err := o.resourceDone("plan", unchanged); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatalf("terminal changed selection: %s", out.String())
		}
		o.all = true
		if err := o.resourceDone("plan", unchanged); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "➤ MacSoftware/example\n  repo\n    ✓ No changes") {
			t.Fatal(out.String())
		}
	}
}

func TestRegionFitsNarrowAndShortTerminals(t *testing.T) {
	for _, width := range []int{10, 30, 48, 80, 120} {
		for _, height := range []int{1, 3, 5, 28} {
			p := newTerminalProgress(io.Discard)
			p.update(activity{label: "Applying reviewed branch", detail: "0e8c221a680f", stage: true})
			for range 8 {
				p.update(activity{scope: "MacSoftware/example", label: "Inspecting a long installer name", stage: true})
				p.update(activity{scope: "MacSoftware/example", label: "Inspecting a long installer name", status: true, elapsed: 2 * time.Second})
			}
			p.update(activity{scope: "MacSoftware/example", label: "Downloading input", detail: "A very long installer filename.pkg", stage: true})
			p.update(activity{scope: "MacSoftware/example", progress: true, current: 10, total: 100, unit: "bytes"})
			history, region := p.view(time.Now().Add(time.Second), width, height)
			if region != "" && strings.Count(region, "\n")+1 > height-1 {
				t.Fatalf("region overflows height %d: %q", height, region)
			}
			for line := range strings.SplitSeq(history+region, "\n") {
				if runewidth.StringWidth(line) >= width {
					t.Fatalf("line overflows width %d: %q", width, line)
				}
			}
		}
	}
}

func TestTallBlockScrollsIntoHistoryWithoutLosingSteps(t *testing.T) {
	const resource = "MacSoftware/example"
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	want := []string{"➤ MacSoftware/example"}
	for i := range 8 {
		label := fmt.Sprintf("Building part %d", i)
		p.update(activity{scope: resource, label: label, stage: true})
		p.update(activity{scope: resource, label: label, status: true, elapsed: 2 * time.Second})
		want = append(want, "  ✓ "+label+" (2s)")
	}
	p.update(activity{scope: resource, label: "Uploading installer", stage: true})
	now := time.UnixMilli(0)
	history, region := p.view(now, 100, 6)
	if history != strings.Join(want[:5], "\n")+"\n" || region != strings.Join(want[5:], "\n")+"\n  ⠋ Uploading installer" {
		t.Fatalf("history:\n%s\nregion:\n%s", history, region)
	}
	if again, _ := p.view(now, 100, 6); again != "" {
		t.Fatalf("history repeated: %q", again)
	}
	p.complete(resource)
	if out.String() != strings.Join(want[5:], "\n")+"\n" {
		t.Fatalf("remaining steps did not follow the block into history: %q", out.String())
	}
}

func TestResourceProgressTakesPrecedenceOverProjectActivity(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	p.update(activity{label: "Resolving catalog", stage: true})
	p.update(activity{label: "Validating operation contracts", stage: true})
	p.update(activity{label: "Validating operation contracts", status: true, elapsed: 2 * time.Second})
	if got := shown(p, time.UnixMilli(0)); got != "⠋ Resolving catalog" {
		t.Fatalf("project step alone: %q", got)
	}
	p.update(activity{scope: "MacSoftware/example", qualifier: "intune", label: "Planning destination", stage: true})
	want := strings.Join([]string{
		"➤ MacSoftware/example",
		"  ⠋ Planning destination (intune)",
	}, "\n")
	if got := shown(p, time.UnixMilli(0)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	p.plain = true
	p.draw(time.Now().Add(3 * time.Second))
	if got := out.String(); !strings.Contains(got, "Planning destination") || strings.Contains(got, "Resolving catalog") {
		t.Fatalf("plain progress competes with resource work: %q", got)
	}
	p.complete("MacSoftware/example")
	if got := shown(p, time.UnixMilli(0)); got != "⠋ Resolving catalog" {
		t.Fatalf("unscoped work did not resume: %s", got)
	}
}

func TestRepeatedStepContinuesItsRow(t *testing.T) {
	const resource = "MacSoftware/example"
	p := newTerminalProgress(io.Discard)
	wait := activity{scope: resource, qualifier: "intune", label: "Waiting for Intune processing", stage: true}
	done := activity{scope: resource, qualifier: "intune", label: "Waiting for Intune processing", status: true}
	p.update(wait)
	done.elapsed = 5 * time.Second
	p.update(done)
	p.update(wait)
	got := shown(p, time.Now().Add(2*time.Second))
	if strings.Count(got, "Waiting") != 1 || strings.Contains(got, "✓") || !strings.HasSuffix(got, "Waiting for Intune processing (intune) (7s)") {
		t.Fatalf("repeated step listed beside its earlier run: %s", got)
	}
	done.elapsed = 2 * time.Second
	p.update(done)
	if got := shown(p, time.Now()); got != "➤ MacSoftware/example\n  ✓ Waiting for Intune processing (intune) (7s)" {
		t.Fatalf("repeated step did not keep one row: %s", got)
	}
	for _, name := range []string{"first.pkg", "second.pkg"} {
		p.update(activity{scope: resource, label: "Inspecting package", detail: name, stage: true})
		p.update(activity{scope: resource, label: "Inspecting package", status: true, elapsed: 2 * time.Second})
	}
	if got := shown(p, time.Now()); strings.Count(got, "Inspecting package") != 2 {
		t.Fatalf("steps on different subjects merged: %s", got)
	}
}

func TestFinishedStepKeepsItsSubjectBesideItsResult(t *testing.T) {
	const resource = "MacSoftware/example"
	p := newTerminalProgress(io.Discard)
	for _, app := range []string{"Suite/First.app", "Suite/Second.app"} {
		p.update(activity{scope: resource, label: "Inspecting signature", detail: app, stage: true})
		p.update(activity{scope: resource, label: "Inspecting signature", detail: "Example Inc.", status: true, elapsed: 2 * time.Second})
	}
	p.update(activity{scope: resource, label: "Downloading input", detail: "example.dmg", stage: true})
	p.update(activity{scope: resource, label: "Downloading input", detail: "example.dmg", status: true, elapsed: 3 * time.Second})
	want := "➤ MacSoftware/example\n" +
		"  ✓ Inspecting signature: Suite/First.app (Example Inc.) (2s)\n" +
		"  ✓ Inspecting signature: Suite/Second.app (Example Inc.) (2s)\n" +
		"  ✓ Downloading input: example.dmg (3s)"
	if got := shown(p, time.Now()); got != want {
		t.Fatalf("finished steps:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnknownTransferShowsCountWithoutPercentage(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{label: "Downloading input", stage: true})
	p.update(activity{progress: true, current: 10, total: -1, unit: "bytes"})
	text := shown(p, time.Now().Add(time.Second))
	if !strings.Contains(text, "10 B") || strings.Contains(text, "%") || strings.Contains(text, "Project") {
		t.Fatal(text)
	}
}

func TestFinalWriteEndsProgressPermanently(t *testing.T) {
	var text bytes.Buffer
	o := newCommandOutput(&text, io.Discard)
	o.interactive = true
	o.progress = newTerminalProgress(io.Discard)
	writer := finalWriter{Writer: &text, output: o}
	if _, err := io.WriteString(writer, "report\n"); err != nil {
		t.Fatal(err)
	}
	if text.String() != "report\n" || o.progress != nil || o.interactive {
		t.Fatal("final write left progress active")
	}
}

func TestOverlappingActivitiesFinishIndependently(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{scope: "example", label: "First operation", stage: true})
	p.update(activity{scope: "example", label: "Second operation", stage: true})
	p.update(activity{scope: "example", label: "First operation", status: true})
	got := shown(p, time.Now().Add(time.Second))
	if !strings.Contains(got, "Second operation") || strings.Contains(got, "First operation") {
		t.Fatal(got)
	}
	p.update(activity{scope: "example", label: "Second operation", status: true})
	if got := shown(p, time.Now().Add(time.Second)); strings.Contains(got, "operation") {
		t.Fatal(got)
	}
}

func TestTransferProgressBelongsToItsInput(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{scope: "example", qualifier: "source", label: "Downloading", stage: true})
	p.update(activity{scope: "example", qualifier: "icon", label: "Downloading", stage: true})
	p.update(activity{scope: "example", qualifier: "source", progress: true, current: 1, total: 2, unit: "bytes"})
	if got := shown(p, time.Now().Add(time.Second)); strings.Contains(got, "1 B") {
		t.Fatal(got)
	}
	p.update(activity{scope: "example", qualifier: "icon", label: "Downloading", status: true})
	if got := shown(p, time.Now().Add(time.Second)); !strings.Contains(got, "1 B / 2 B") {
		t.Fatal(got)
	}
}

func TestPlainProgressIsDelayedSparseAndStopsWithItsOperation(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	p.plain = true
	p.update(activity{scope: "MacSoftware/example", label: "Downloading input", stage: true, detail: "example.pkg"})
	now := time.Now()
	p.draw(now)
	if out.Len() != 0 {
		t.Fatal("fast activity printed")
	}
	p.draw(now.Add(3 * time.Second))
	first := out.String()
	if !strings.Contains(first, "MacSoftware/example: Downloading input: example.pkg") || strings.Contains(first, "\x1b") {
		t.Fatalf("milestone: %q", first)
	}
	p.update(activity{scope: "MacSoftware/example", progress: true, current: 1, total: 4, unit: "bytes"})
	p.draw(now.Add(4 * time.Second))
	if out.String() != first {
		t.Fatal("chunk produced a line")
	}
	p.draw(now.Add(34 * time.Second))
	if !strings.Contains(out.String(), "1 B / 4 B") {
		t.Fatalf("no periodic observation: %s", out.String())
	}
	p.update(activity{scope: "MacSoftware/example", label: "Downloading input", status: true})
	length := out.Len()
	p.draw(now.Add(time.Minute))
	p.stop()
	if out.Len() != length {
		t.Fatal("finished work printed")
	}
}

func TestScrolledBlockFinishesWhenScopeChangesOrStops(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop=%t", stop), func(t *testing.T) {
			var out bytes.Buffer
			p := newTerminalProgress(&out)
			for i := range 5 {
				label := fmt.Sprintf("Part %d", i)
				p.update(activity{scope: "MacSoftware/example", label: label, stage: true})
				p.update(activity{scope: "MacSoftware/example", label: label, status: true, elapsed: time.Second})
			}
			history, _ := p.view(time.Now(), 100, 3)
			out.WriteString(history)
			if stop {
				p.stop()
			} else {
				p.update(activity{scope: "MacSoftware/next", label: "Downloading", stage: true})
			}
			for i := range 5 {
				want := fmt.Sprintf("✓ Part %d", i)
				if strings.Count(out.String(), want) != 1 {
					t.Fatalf("lost or duplicated %s: %q", want, out.String())
				}
			}
		})
	}
}

type progressWrites struct{ writes []string }

func (w *progressWrites) Write(data []byte) (int, error) {
	w.writes = append(w.writes, string(data))
	return len(data), nil
}

func TestStderrNoticeAndProgressRedrawShareAWrite(t *testing.T) {
	out := &progressWrites{}
	p := newTerminalProgress(out)
	p.update(activity{scope: "MacSoftware/example", label: "Downloading", stage: true})
	p.draw(time.Now())
	out.writes = nil
	if err := p.write(out, "! check configuration\n"); err != nil {
		t.Fatal(err)
	}
	if len(out.writes) != 1 || !strings.Contains(out.writes[0], "! check configuration\n➤ MacSoftware/example\n") {
		t.Fatalf("notice exposes separate clear/redraw frames: %q", out.writes)
	}
}

func TestPipelinePhasesKeepTransferAndDestinationHistory(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	const resource = "MacSoftware/example"
	step := func(phase, label, detail string, elapsed time.Duration) {
		p.update(activity{scope: resource, phase: phase, label: label, detail: detail, stage: true})
		p.update(activity{scope: resource, phase: phase, label: label, elapsed: elapsed, status: true})
	}
	step("Acquire", "Downloading source", "vendor.pkg", 3*time.Second)
	step("Prepare", "Inspecting package", "vendor.pkg", 2*time.Second)
	for _, destination := range []string{"first", "second"} {
		phase := "Publish → " + destination
		p.update(activity{scope: resource, phase: phase, label: "Uploading installer", detail: "example.pkg", stage: true})
		p.update(activity{scope: resource, phase: phase, progress: true, current: 4 << 20, total: 4 << 20, unit: "bytes"})
		p.update(activity{scope: resource, phase: phase, label: "Uploading installer", status: true, elapsed: 100 * time.Millisecond})
		step(phase, "Finalizing upload", "example.pkg", 3*time.Second)
	}
	p.update(activity{scope: resource, phase: "Publish → second", label: "Saving package", stage: true})
	want := strings.Join([]string{
		"➤ MacSoftware/example",
		"  Acquire",
		"    ✓ Downloading source: vendor.pkg (3s)",
		"",
		"  Prepare",
		"    ✓ Inspecting package: vendor.pkg (2s)",
		"",
		"  Publish → first",
		"    ✓ Uploading installer: example.pkg 4.0 MiB",
		"    ✓ Finalizing upload: example.pkg (3s)",
		"",
		"  Publish → second",
		"    ✓ Uploading installer: example.pkg 4.0 MiB",
		"    ✓ Finalizing upload: example.pkg (3s)",
		"    ⠋ Saving package",
	}, "\n")
	if got := shown(p, time.UnixMilli(0)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPhaseHeadingsAndStepsScrollOnce(t *testing.T) {
	for _, height := range []int{1, 3, 6, 10} {
		t.Run(strconv.Itoa(height), func(t *testing.T) {
			var out bytes.Buffer
			p := newTerminalProgress(&out)
			const resource = "MacSoftware/example"
			want := "➤ " + resource + "\n"
			var history strings.Builder
			for i, phase := range []string{"Acquire", "Prepare", "Publish → repo"} {
				if i > 0 {
					want += "\n"
				}
				want += "  " + phase + "\n"
				for i := range 4 {
					label := fmt.Sprintf("Step %d", i)
					p.update(activity{scope: resource, phase: phase, label: label, stage: true})
					scrolled, _ := p.view(time.Now(), 50, height)
					history.WriteString(scrolled)
					p.update(activity{scope: resource, phase: phase, label: label, status: true, elapsed: 2 * time.Second})
					want += "    ✓ " + label + " (2s)\n"
				}
			}
			scrolled, _ := p.view(time.Now(), 50, height)
			history.WriteString(scrolled)
			p.complete(resource)
			if got := history.String() + out.String(); got != want {
				t.Fatalf("history:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestFastCachedPreparationDoesNotLeaveEmptyPhases(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{scope: "MacSoftware/example", phase: "Prepare", label: "Checking preparation cache", stage: true})
	p.update(activity{scope: "MacSoftware/example", phase: "Prepare", label: "Checking preparation cache", status: true, detail: "cached"})
	if got := shown(p, time.Now()); got != "➤ MacSoftware/example" {
		t.Fatalf("empty phase: %s", got)
	}
}

func TestPhaseContextIsSharedByTerminalAndPlainProgress(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalProgress(&out)
	attrs := []slog.Attr{slog.String("resource", "MacSoftware/example"), slog.String("phase", "Publish"), slog.String("destination", "woodstar")}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "Uploading installer", 0)
	record.AddAttrs(slog.Bool("stage", true), slog.String("detail", "example.pkg"))
	p.update(readActivity(record, attrs))
	if got := shown(p, time.UnixMilli(0)); got != "➤ MacSoftware/example\n  Publish → woodstar\n    ⠋ Uploading installer: example.pkg" {
		t.Fatalf("phase or destination lost: %s", got)
	}
	p.plain = true
	p.draw(time.Now().Add(3 * time.Second))
	if got := out.String(); !strings.Contains(got, "MacSoftware/example: Publish → woodstar: Uploading installer: example.pkg") || strings.Contains(got, "\x1b") {
		t.Fatalf("plain progress lost context: %q", got)
	}
}
