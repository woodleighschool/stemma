package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/woodleighschool/stemma/internal/engine"
)

func TestProgressShowsCurrentWorkAfterDelay(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{scope: "MacSoftware/example", label: "Acquiring input", stage: true})
	p.update(activity{scope: "MacSoftware/example", label: "Downloading input", detail: "example.pkg", stage: true})
	p.update(activity{scope: "MacSoftware/example", progress: true, current: 1 << 20, total: 4 << 20, unit: "bytes"})
	if got := p.view(time.Now(), 100, 28); got != "" {
		t.Fatalf("fast work flashed: %q", got)
	}
	got := p.view(time.Now().Add(time.Second), 100, 28)
	for _, want := range []string{"MacSoftware/example", "Downloading input", "example.pkg", "1.0 MiB / 4.0 MiB"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Acquiring") || strings.ContainsAny(got, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
		t.Fatalf("duplicated activity: %s", got)
	}
	p.update(activity{scope: "MacSoftware/example", label: "Downloading input", status: true})
	p.update(activity{scope: "MacSoftware/example", label: "Acquiring input", status: true})
	if got := p.view(time.Now().Add(time.Second), 100, 28); got != "" {
		t.Fatalf("completed work retained: %s", got)
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

func TestProgressFitsNarrowAndShortTerminals(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	for _, scope := range []string{"MacSoftware/one", "MacSoftware/two", "MacSoftware/three", "MacSoftware/four"} {
		p.update(activity{scope: scope, label: "Downloading input", detail: "A very long installer filename.pkg", stage: true})
		p.update(activity{scope: scope, progress: true, current: 10, total: 100, unit: "bytes"})
	}
	for _, width := range []int{10, 30, 48, 80, 120} {
		for _, height := range []int{1, 3, 5, 28} {
			text := p.view(time.Now().Add(time.Second), width, height)
			if text == "" {
				continue
			}
			lines := strings.Split(text, "\n")
			if len(lines) > min(6, height-1) {
				t.Fatalf("region overflows height %d: %q", height, text)
			}
			for _, line := range lines {
				if runewidth.StringWidth(line) >= width {
					t.Fatalf("region overflows width %d: %q", width, line)
				}
			}
		}
	}
}

func TestUnknownTransferShowsCountWithoutPercentage(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{label: "Downloading input", stage: true})
	p.update(activity{progress: true, current: 10, total: -1, unit: "bytes"})
	text := p.view(time.Now().Add(time.Second), 100, 28)
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
	got := p.view(time.Now().Add(time.Second), 100, 28)
	if !strings.Contains(got, "Second operation") || strings.Contains(got, "First operation") {
		t.Fatal(got)
	}
	p.update(activity{scope: "example", label: "Second operation", status: true})
	if got := p.view(time.Now().Add(time.Second), 100, 28); got != "" {
		t.Fatal(got)
	}
}

func TestTransferProgressBelongsToItsInput(t *testing.T) {
	p := newTerminalProgress(io.Discard)
	p.update(activity{scope: "example", qualifier: "source", label: "Downloading", stage: true})
	p.update(activity{scope: "example", qualifier: "icon", label: "Downloading", stage: true})
	p.update(activity{scope: "example", qualifier: "source", progress: true, current: 1, total: 2, unit: "bytes"})
	if got := p.view(time.Now().Add(time.Second), 100, 28); strings.Contains(got, "1 B") {
		t.Fatal(got)
	}
	p.update(activity{scope: "example", qualifier: "icon", label: "Downloading", status: true})
	if got := p.view(time.Now().Add(time.Second), 100, 28); !strings.Contains(got, "1 B / 2 B") {
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
	if !strings.Contains(first, "MacSoftware/example: Downloading input · example.pkg") || strings.Contains(first, "\x1b") {
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
