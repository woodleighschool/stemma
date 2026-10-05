package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

type activity struct {
	stage, progress, status bool
	// label is the stage message; qualifier names the input, destination or
	// plugin it works for.
	label, qualifier, scope, detail, unit string
	current, total                        int64
	elapsed                               time.Duration
	err                                   string
}

// name is the stage message with what it works for.
func (a activity) name() string {
	if a.qualifier == "" {
		return a.label
	}
	return a.label + " (" + a.qualifier + ")"
}

func readActivity(record slog.Record, attrs []slog.Attr) activity {
	a := activity{label: record.Message}
	values := make(map[string]string)
	read := func(attr slog.Attr) bool {
		switch attr.Key {
		case "stage":
			a.stage = attr.Value.Kind() == slog.KindBool && attr.Value.Bool()
		case "progress":
			a.progress = attr.Value.Kind() == slog.KindBool && attr.Value.Bool()
		case "stage_result":
			a.status = attr.Value.Kind() == slog.KindBool && attr.Value.Bool()
		case "current":
			a.current = activityCount(attr.Value)
		case "total":
			a.total = activityCount(attr.Value)
		case "elapsed":
			a.elapsed = time.Duration(activityCount(attr.Value))
		case "unit":
			a.unit = attr.Value.String()
		case "detail":
			a.detail = attr.Value.String()
		case "error":
			if attr.Value.Any() != nil {
				a.err = attr.Value.String()
			}
		case "resource", "input", "destination", "plugin":
			values[attr.Key] = attr.Value.String()
		}
		return true
	}
	for _, attr := range attrs {
		read(attr)
	}
	record.Attrs(read)
	a.scope = values["resource"]
	var qualifiers []string
	for _, key := range []string{"input", "destination", "plugin"} {
		if values[key] != "" {
			qualifiers = append(qualifiers, values[key])
		}
	}
	a.qualifier = strings.Join(qualifiers, ", ")
	return a
}

func activityCount(value slog.Value) int64 {
	if value.Kind() == slog.KindInt64 {
		return value.Int64()
	}
	if value.Kind() == slog.KindDuration {
		return int64(value.Duration())
	}
	return 0
}

const progressDelay = 250 * time.Millisecond

type progressLine struct {
	activity

	started   time.Time
	announced time.Time
}

type progressGroup struct {
	scope string
	rows  []progressLine
}

// terminalProgress owns a bounded transient region. Its clock, activity
// updates and persistent writes share one lock: no renderer can repaint a
// stale frame after a report has begun scrolling the terminal.
type terminalProgress struct {
	mu           sync.Mutex
	out          io.Writer
	style        textStyle
	groups       []*progressGroup
	shown        int
	last         string
	quit, exited chan struct{}
	stopped      bool
	plain        bool
}

func newTerminalProgress(out io.Writer) *terminalProgress {
	p := &terminalProgress{out: out, style: newTextStyle(out)}
	if terminalOutput(out) {
		p.quit, p.exited = make(chan struct{}), make(chan struct{})
		go p.run()
	}
	return p
}

// startPlain uses the same operation lifetime but emits sparse append-only
// milestones. Fast phases stay quiet, and transfer chunks never become logs.
func (p *terminalProgress) startPlain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plain = true
	if p.quit == nil {
		p.quit, p.exited = make(chan struct{}), make(chan struct{})
		go p.run()
	}
}

func (p *terminalProgress) run() {
	defer close(p.exited)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			p.mu.Lock()
			if !p.stopped {
				p.draw(now)
			}
			p.mu.Unlock()
		case <-p.quit:
			return
		}
	}
}

func (p *terminalProgress) update(a activity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	index := slices.IndexFunc(p.groups, func(g *progressGroup) bool { return g.scope == a.scope })
	if index < 0 {
		if !a.stage {
			return
		}
		index = len(p.groups)
		p.groups = append(p.groups, &progressGroup{scope: a.scope})
	}
	group := p.groups[index]
	switch {
	case a.stage:
		group.rows = append(group.rows, progressLine{activity: a, started: time.Now()})
	case a.progress:
		for i, row := range slices.Backward(group.rows) {
			if row.qualifier == a.qualifier {
				group.rows[i].current, group.rows[i].total, group.rows[i].unit = a.current, a.total, a.unit
				break
			}
		}
	case a.status:
		for i, row := range slices.Backward(group.rows) {
			if row.label == a.label && row.qualifier == a.qualifier {
				group.rows = slices.Delete(group.rows, i, i+1)
				break
			}
		}
		if len(group.rows) == 0 {
			p.groups = slices.Delete(p.groups, index, index+1)
		}
	}
}

func (p *terminalProgress) complete(scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.groups = slices.DeleteFunc(p.groups, func(g *progressGroup) bool { return g.scope == scope })
}

// write suspends the live region while the original stream receives its
// persistent text. The next clock tick can render only still-active work.
func (p *terminalProgress) write(out io.Writer, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	_, err := io.WriteString(out, text)
	return err
}

func (p *terminalProgress) stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.clear()
	p.groups = nil
	if p.quit != nil {
		close(p.quit)
	}
	p.mu.Unlock()
	if p.exited != nil {
		<-p.exited
	}
}

func (p *terminalProgress) size() (int, int) {
	if file, ok := p.out.(*os.File); ok {
		if width, height, err := term.GetSize(int(file.Fd())); err == nil && width > 0 && height > 0 {
			return width, height
		}
	}
	return 100, 28
}

// view shows one current operation per scope, never its stack of wrappers.
// Completed steps belong to outcomes, not the live region.
func (p *terminalProgress) view(now time.Time, width, height int) string {
	var lines []string
	budget := min(6, max(0, height-1))
	frame := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}[(now.UnixMilli()/100)%10]
	for index, group := range p.groups {
		if len(group.rows) == 0 {
			continue
		}
		row := group.rows[len(group.rows)-1]
		if now.Sub(row.started) < progressDelay {
			continue
		}
		needed := 1
		if group.scope != "" {
			needed++
		}
		if len(lines)+needed > budget || (index < len(p.groups)-1 && len(lines)+needed == budget && budget >= 3) {
			if len(lines) < budget {
				more := fmt.Sprintf("… %d more active operations", len(p.groups)-index)
				lines = append(lines, runewidth.Truncate(more, max(0, width-1), "…"))
			}
			break
		}
		if group.scope != "" {
			lines = append(lines, p.style.paint(runewidth.Truncate(cleanLine(group.scope), max(0, width-1), "…"), color.Bold, color.FgHiMagenta))
		}
		lines = append(lines, progressText(p.style, &row, width, now, frame))
	}
	return strings.Join(lines, "\n")
}

func (p *terminalProgress) draw(now time.Time) {
	if p.plain {
		for _, group := range p.groups {
			if len(group.rows) == 0 {
				continue
			}
			row := &group.rows[len(group.rows)-1]
			if now.Sub(row.started) < 2*time.Second || !row.announced.IsZero() && now.Sub(row.announced) < 30*time.Second {
				continue
			}
			line := cleanLine(row.name())
			if group.scope != "" {
				line = cleanLine(group.scope) + ": " + line
			}
			if row.detail != "" {
				line += " · " + cleanLine(row.detail)
			}
			if row.total > 0 && row.unit == "bytes" {
				line += fmt.Sprintf(" · %s / %s", humanize.IBytes(uint64(max(0, row.current))), humanize.IBytes(uint64(row.total)))
			}
			_, _ = fmt.Fprintf(p.out, "%s (%s)\n", line, now.Sub(row.started).Round(time.Second))
			row.announced = now
		}
		return
	}

	width, height := p.size()
	text := p.view(now, width, height)
	if text == p.last {
		return
	}
	// Erase and repaint in one write so frames do not expose a blank region.
	_, _ = io.WriteString(p.out, p.erase()+text)
	p.last = text
	p.shown = 0
	if text != "" {
		p.shown = strings.Count(text, "\n") + 1
	}
}

func (p *terminalProgress) erase() string {
	if p.shown == 0 {
		return ""
	}
	if p.shown == 1 {
		return "\r\x1b[J"
	}
	return fmt.Sprintf("\r\x1b[%dA\x1b[J", p.shown-1)
}

func (p *terminalProgress) clear() {
	if p.shown == 0 {
		return
	}
	_, _ = io.WriteString(p.out, p.erase())
	p.shown, p.last = 0, ""
}

// barWidth is the width of a transfer's bar, which is dropped first when a
// row does not fit.
const barWidth = 20

// segment is rendered text with the plain text that sets its width.
type segment struct{ plain, painted string }

func progressText(style textStyle, line *progressLine, width int, now time.Time, frame string) string {
	mark := frame
	if line.total > 0 {
		mark = ""
	}
	const indent = "  "
	attribute := color.FgHiCyan
	faint := func(text string) segment { return segment{text, style.paint(text, color.Faint)} }
	var bar, counts segment
	if line.unit != "" {
		counts = faint(amount(line.current, line.unit))
		if line.total > 0 {
			counts = faint(amounts(line.current, line.total, line.unit))
			filled := int(min(barWidth, max(0, barWidth*line.current/line.total)))
			done, left := strings.Repeat("━", filled), strings.Repeat("─", barWidth-filled)
			bar = segment{done + left, style.paint(done, color.FgHiCyan) + style.paint(left, color.Faint)}
		}
	}
	var elapsed segment
	if duration := now.Sub(line.started).Round(time.Second); duration > 0 {
		elapsed = faint("(" + duration.String() + ")")
	}
	label, detail := cleanLine(line.name()), cleanLine(line.detail)
	available := max(0, width-runewidth.StringWidth(indent+mark)-2)
	measure := func(parts ...segment) int {
		total := 0
		for _, part := range parts {
			if part.plain != "" {
				total += 2 + runewidth.StringWidth(part.plain)
			}
		}
		return total
	}
	// Drop the bar, then shorten the detail, then drop the counts and the
	// time; the label is shortened last.
	room := available - runewidth.StringWidth(label)
	if measure(faint(detail), bar, counts, elapsed) > room {
		bar = segment{}
	}
	if over := measure(faint(detail), counts, elapsed) - room; over > 0 && detail != "" {
		detail = runewidth.Truncate(detail, max(0, runewidth.StringWidth(detail)-over), "…")
		if runewidth.StringWidth(detail) < 8 {
			detail = ""
		}
	}
	if measure(faint(detail), counts, elapsed) > room {
		counts = segment{}
	}
	if measure(faint(detail), elapsed) > room {
		elapsed = segment{}
	}
	parts := []segment{faint(detail), bar, counts, elapsed}
	tail := "..."
	if available-measure(parts...) < len(tail) {
		tail = ""
	}
	var text strings.Builder
	text.WriteString(indent + style.paint(mark, attribute) + " " + runewidth.Truncate(label, max(0, available-measure(parts...)), tail))
	for _, part := range parts {
		if part.plain != "" {
			text.WriteString("  " + part.painted)
		}
	}
	return text.String()
}

// amount formats a transfer count in its unit.
func amount(count int64, unit string) string {
	if unit == "bytes" {
		return humanize.IBytes(uint64(max(0, count)))
	}
	return humanize.Comma(count) + " " + unit
}

// amounts formats progress toward a known total.
func amounts(current, total int64, unit string) string {
	if unit == "bytes" {
		return amount(current, unit) + " / " + amount(total, unit)
	}
	return humanize.Comma(current) + " / " + amount(total, unit)
}

func cleanLine(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
