package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/woodleighschool/stemma/internal/engine"
)

// shown returns the live region in a roomy terminal.
func shown(p *progressState, now time.Time) string {
	return p.view(now, 100, 28)
}

func TestPersistentResultsClearProgressAndKeepTheirStream(t *testing.T) {
	var stdout, stderr bytes.Buffer
	p := newTerminalProgress(t.Context(), &stderr, false, nil)
	p.update(activity{scope: "MacSoftware/example", label: "Preparing", stage: true})
	p.complete("MacSoftware/example")
	report := strings.Repeat("a long report line\n", 80)
	if err := p.write(&stdout, report); err != nil {
		t.Fatal(err)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != report {
		t.Fatalf("stdout lost report: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "a long report") || strings.Contains(stderr.String(), "\x1b") {
		t.Fatalf("report mixed with progress: %q", stderr.String())
	}
}

func TestHumanSelectionDoesNotDependOnTerminal(t *testing.T) {
	unchanged := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Status: engine.StatusUnchanged, Publications: []engine.Publication{{Destination: "repo", Status: engine.StatusUnchanged}}}
	for _, interactive := range []bool{true, false} {
		var out bytes.Buffer
		o := newCommandOutput(&out, io.Discard)
		o.interactive = interactive
		if err := o.resourceDone("plan", unchanged); err != nil {
			t.Fatal(err)
		}
		if out.String() != "✓ MacSoftware/example: unchanged\n" {
			t.Fatal(out.String())
		}
	}
}

func TestRegionFitsNarrowAndShortTerminals(t *testing.T) {
	for _, width := range []int{10, 30, 48, 80, 120} {
		for _, height := range []int{1, 3, 5, 28} {
			p := &progressState{}
			p.update(activity{label: "Applying reviewed branch", detail: "0e8c221a680f", stage: true})
			for i := range 8 {
				p.update(activity{scope: "MacSoftware/example", label: "Inspecting a long installer name", item: fmt.Sprintf("component-%d.pkg", i), stage: true})
				p.update(activity{scope: "MacSoftware/example", label: "Inspecting a long installer name", status: true})
			}
			p.update(activity{scope: "MacSoftware/example", label: "Verifying signature", item: "A very long installer filename.pkg", stage: true})
			p.update(activity{scope: "MacSoftware/example", progress: true, current: 10, total: 100, unit: "bytes"})
			region := p.view(time.Now().Add(time.Second), width, height)
			if region != "" && strings.Count(region, "\n")+1 > height-1 {
				t.Fatalf("region overflows height %d: %q", height, region)
			}
			for line := range strings.SplitSeq(region, "\n") {
				if runewidth.StringWidth(line) >= width {
					t.Fatalf("line overflows width %d: %q", width, line)
				}
			}
		}
	}
}

func TestResourceProgressTakesPrecedenceOverProjectActivity(t *testing.T) {
	var out bytes.Buffer
	p := &progressState{style: newTextStyle(&out)}
	p.update(activity{label: "Resolving catalog", stage: true})
	p.update(activity{label: "Checking resource configuration", stage: true})
	p.update(activity{label: "Checking resource configuration", status: true})
	if got := shown(p, time.UnixMilli(0)); got != "⠋ Resolving catalog" {
		t.Fatalf("project step alone: %q", got)
	}
	p.update(activity{scope: "MacSoftware/example", qualifier: "intune", label: "Planning destination", stage: true})
	want := strings.Join([]string{
		"MacSoftware/example:",
		"  intune:",
		"    ⠋ Planning destination",
	}, "\n")
	if got := shown(p, time.UnixMilli(0)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	out.WriteString(p.plain(time.Now().Add(3 * time.Second)))
	if got := out.String(); !strings.Contains(got, "Planning destination") || strings.Contains(got, "Resolving catalog") {
		t.Fatalf("plain progress competes with resource work: %q", got)
	}
	p.complete("MacSoftware/example")
	if got := shown(p, time.UnixMilli(0)); got != "⠋ Resolving catalog" {
		t.Fatalf("unscoped work did not resume: %s", got)
	}
}

func TestUnknownTransferShowsCountWithoutPercentage(t *testing.T) {
	p := &progressState{}
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
	o.progress = newTerminalProgress(t.Context(), io.Discard, false, nil)
	writer := finalWriter{Writer: &text, output: o}
	if _, err := io.WriteString(writer, "report\n"); err != nil {
		t.Fatal(err)
	}
	if text.String() != "report\n" || o.progress != nil || o.interactive {
		t.Fatal("final write left progress active")
	}
}

func TestOverlappingActivitiesFinishIndependently(t *testing.T) {
	p := &progressState{}
	p.update(activity{scope: "example", qualifier: "first", label: "First operation", stage: true})
	p.update(activity{scope: "example", qualifier: "second", label: "Second operation", stage: true})
	p.update(activity{scope: "example", qualifier: "first", label: "First operation", status: true})
	got := shown(p, time.UnixMilli(0))
	if !strings.Contains(got, "second:\n    ⠋ Second operation") || !strings.Contains(got, "first:\n    ✓ First operation") {
		t.Fatal(got)
	}
	p.update(activity{scope: "example", qualifier: "second", label: "Second operation", status: true})
	p.complete("example")
	if got := shown(p, time.Now().Add(time.Second)); strings.Contains(got, "operation") {
		t.Fatal(got)
	}
}

func TestTransferProgressBelongsToItsInput(t *testing.T) {
	p := &progressState{}
	p.update(activity{scope: "example", qualifier: "source", label: "Downloading", stage: true})
	p.update(activity{scope: "example", qualifier: "icon", label: "Downloading", stage: true})
	p.update(activity{scope: "example", qualifier: "source", progress: true, current: 1, total: 2, unit: "bytes"})
	if got := shown(p, time.Now().Add(time.Second)); !strings.Contains(got, "1 B / 2 B") || !strings.Contains(got, "icon:\n    ") {
		t.Fatal(got)
	}
	p.update(activity{scope: "example", qualifier: "icon", label: "Downloading", status: true})
	if got := shown(p, time.Now().Add(time.Second)); !strings.Contains(got, "1 B / 2 B") {
		t.Fatal(got)
	}
}

func TestPlainProgressIsDelayedSparseAndStopsWithItsOperation(t *testing.T) {
	var out bytes.Buffer
	p := &progressState{style: newTextStyle(&out)}
	p.update(activity{scope: "MacSoftware/example", label: "Downloading input", stage: true, detail: "example.pkg"})
	now := time.Now()
	out.WriteString(p.plain(now))
	if out.Len() != 0 {
		t.Fatal("fast activity printed")
	}
	out.WriteString(p.plain(now.Add(3 * time.Second)))
	first := out.String()
	if !strings.Contains(first, "MacSoftware/example: Downloading input: example.pkg") || strings.Contains(first, "\x1b") {
		t.Fatalf("milestone: %q", first)
	}
	p.update(activity{scope: "MacSoftware/example", progress: true, current: 1, total: 4, unit: "bytes"})
	out.WriteString(p.plain(now.Add(4 * time.Second)))
	if out.String() != first {
		t.Fatal("chunk produced a line")
	}
	out.WriteString(p.plain(now.Add(34 * time.Second)))
	if !strings.Contains(out.String(), "1 B / 4 B") {
		t.Fatalf("no periodic observation: %s", out.String())
	}
	p.update(activity{scope: "MacSoftware/example", label: "Downloading input", status: true})
	length := out.Len()
	out.WriteString(p.plain(now.Add(time.Minute)))
	if out.Len() != length {
		t.Fatal("finished work printed")
	}
}

func TestDestinationOwnershipIsSharedByTerminalAndPlainProgress(t *testing.T) {
	var out bytes.Buffer
	p := &progressState{style: newTextStyle(&out)}
	attrs := []slog.Attr{slog.String("resource", "MacSoftware/example"), slog.String("phase", "Publish"), slog.String("destination", "woodstar")}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "Uploading installer", 0)
	record.AddAttrs(slog.Bool("stage", true), slog.String("detail", "example.pkg"))
	p.update(readActivity(record, attrs))
	if got := shown(p, time.UnixMilli(0)); got != "MacSoftware/example:\n  woodstar:\n    ⠋ Uploading installer: example.pkg" {
		t.Fatalf("phase or destination lost: %s", got)
	}
	out.WriteString(p.plain(time.Now().Add(3 * time.Second)))
	if got := out.String(); !strings.Contains(got, "MacSoftware/example: woodstar: Uploading installer: example.pkg") || strings.Contains(got, "\x1b") {
		t.Fatalf("plain progress lost context: %q", got)
	}
}

func TestProgressExpandsStagesAndCollapsesToResult(t *testing.T) {
	p := &progressState{}
	const resource = "MacSoftware/example"
	now := time.UnixMilli(0)
	p.update(activity{scope: resource, phase: "Acquire", label: "Downloading", stage: true})
	p.update(activity{scope: resource, phase: "Acquire", progress: true, current: 1 << 20, total: 4 << 20, unit: "bytes"})
	if got := p.view(now, 48, 28); !strings.Contains(got, "━━━━") || !strings.Contains(got, "25%") {
		t.Fatalf("transfer bar missing: %s", got)
	}
	p.update(activity{scope: resource, phase: "Acquire", label: "Downloading", status: true})
	p.update(activity{scope: resource, phase: "Prepare", label: "Inspecting", stage: true})
	p.update(activity{scope: resource, phase: "Prepare", progress: true, current: 120, unit: "entries"})
	got := shown(p, now)
	for _, want := range []string{"MacSoftware/example", "✓ Downloading", "⠋ Inspecting", "120 entries", "━━━"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	p.complete(resource)
	if got := shown(p, now); got != "" {
		t.Fatalf("tree survived result: %s", got)
	}
}

func TestPreparedResourcesWaitOffscreenUntilTheyResume(t *testing.T) {
	p := &progressState{}
	for i := range 100 {
		scope := fmt.Sprintf("MacSoftware/prepared-%03d", i)
		p.update(activity{scope: scope, phase: "Prepare", label: "Inspecting installer", stage: true})
		p.update(activity{scope: scope, phase: "Prepare", label: "Inspecting installer", status: true})
	}
	if got := shown(p, time.UnixMilli(0)); got != "" {
		t.Fatalf("idle resources stayed on screen: %s", got)
	}
	p.update(activity{scope: "MacSoftware/prepared-042", phase: "Publish", label: "Uploading installer", stage: true})
	p.update(activity{scope: "MacSoftware/prepared-042", phase: "Publish", progress: true, current: 5, total: 10, unit: "bytes"})
	got := shown(p, time.UnixMilli(0))
	if strings.Count(got, "MacSoftware/prepared-") != 1 || !strings.Contains(got, "✓ Inspecting installer") || !strings.Contains(got, "50%") {
		t.Fatalf("resumed resource lost its history or progress: %s", got)
	}
}

func TestNestedAcquisitionHasOneLoader(t *testing.T) {
	p := &progressState{}
	const scope = "MacSoftware/example"
	p.update(activity{scope: scope, qualifier: "source", label: "Acquiring input", stage: true})
	p.update(activity{scope: scope, qualifier: "source", label: "Discovering source release", detail: "example.test", stage: true})
	got := shown(p, time.UnixMilli(0))
	want := "MacSoftware/example:\n  ⠋ Discovering source release: example.test"
	if got != want {
		t.Fatalf("nested acquisition: %q", got)
	}
	p.update(activity{scope: scope, qualifier: "source", label: "Discovering source release", status: true})
	p.update(activity{scope: scope, qualifier: "source", label: "Downloading input", stage: true})
	p.update(activity{scope: scope, qualifier: "source", progress: true, current: 5, total: 10, unit: "bytes"})
	got = shown(p, time.UnixMilli(0))
	if strings.Count(got, "⠋") != 1 || strings.Contains(got, "Acquiring") || strings.Contains(got, "(source)") || !strings.Contains(got, "50%") {
		t.Fatalf("nested download: %s", got)
	}
}

func TestShortProgressKeepsResourceAndDestination(t *testing.T) {
	p := &progressState{}
	p.update(activity{scope: "MacSoftware/example", phase: "Publish", qualifier: "intune", label: "Uploading installer", stage: true, current: 20, total: 100, unit: "bytes"})
	got := p.view(time.Now(), 80, 3)
	for _, want := range []string{"MacSoftware/example", "intune", "Uploading installer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("short display lost %q: %s", want, got)
		}
	}
	if strings.Count(got, "\n") > 1 {
		t.Fatalf("display exceeded height: %q", got)
	}
}
