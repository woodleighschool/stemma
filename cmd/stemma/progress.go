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
	label, qualifier, scope, phase, detail, unit string
	current, total                               int64
	elapsed                                      time.Duration
	err                                          string
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
		case "resource", "phase", "input", "destination", "plugin":
			values[attr.Key] = attr.Value.String()
		}
		return true
	}
	for _, attr := range attrs {
		read(attr)
	}
	record.Attrs(read)
	a.scope = values["resource"]
	a.phase = values["phase"]
	if a.phase != "" && values["destination"] != "" {
		a.phase += " → " + values["destination"]
		delete(values, "destination")
	}
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

// keptStep is how long a step runs before it stays listed once finished.
const keptStep = time.Second

type progressLine struct {
	activity

	// subject is the detail the step started with, which names what it works
	// on. With the label and qualifier it identifies the step's line.
	subject   string
	started   time.Time
	announced time.Time
	finished  bool
	// enclosing marks a step that another step ran inside.
	enclosing bool
}

type progressGroup struct {
	scope string
	rows  []progressLine
	// scrolled marks a block whose heading has scrolled into history.
	scrolled bool
	// scrolledPhase is the last phase heading already in history.
	scrolledPhase string
	// A separator can scroll before the heading it introduces.
	scrolledGap bool
}

// running returns the innermost unfinished step. The steps of one scope run
// one at a time, so a step that starts while another runs is inside it.
func (g *progressGroup) running() *progressLine {
	for i := len(g.rows) - 1; i >= 0; i-- {
		if !g.rows[i].finished {
			return &g.rows[i]
		}
	}
	return nil
}

// scrollLine forgets the block's top line, which history now holds.
func (g *progressGroup) scrollLine(phase string, gap bool) {
	if !g.scrolled {
		g.scrolled = true
		return
	}
	if gap {
		g.scrolledGap = true
		return
	}
	if phase != "" {
		g.scrolledPhase = phase
		g.scrolledGap = false
		return
	}
	i := slices.IndexFunc(g.rows, func(row progressLine) bool { return row.finished })
	g.rows = slices.Delete(g.rows, i, i+1)
}

// terminalProgress owns a transient region: a block per resource with its
// heading, its finished steps worth keeping and the step running now.
// Project activity yields to running resource work. A block stays from the resource's first
// step until its report replaces it or another scope starts work. The clock,
// activity updates and persistent writes share one lock: no renderer can
// repaint a stale frame after a report has begun scrolling the terminal.
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
	var history strings.Builder
	if a.stage {
		for i := 0; i < len(p.groups); {
			group := p.groups[i]
			if group.scope != a.scope && group.running() == nil {
				history.WriteString(p.remaining(group))
				p.groups = slices.Delete(p.groups, i, i+1)
			} else {
				i++
			}
		}
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
		for i := range group.rows {
			group.rows[i].enclosing = group.rows[i].enclosing || !group.rows[i].finished && group.rows[i].phase == a.phase
		}
		row := progressLine{activity: a, subject: a.detail, started: time.Now()}
		// A step that runs again carries on from its earlier row and time
		// instead of listing itself twice.
		earlier := slices.IndexFunc(group.rows, func(other progressLine) bool {
			return other.finished && other.phase == a.phase && other.label == a.label && other.qualifier == a.qualifier && other.subject == a.detail
		})
		if earlier >= 0 {
			row.elapsed = group.rows[earlier].elapsed
			group.rows = slices.Delete(group.rows, earlier, earlier+1)
		}
		group.rows = append(group.rows, row)
	case a.progress:
		for i := len(group.rows) - 1; i >= 0; i-- {
			if row := &group.rows[i]; !row.finished && row.phase == a.phase && row.qualifier == a.qualifier {
				row.current, row.total, row.unit = a.current, a.total, a.unit
				break
			}
		}
	case a.status:
		for i := len(group.rows) - 1; i >= 0; i-- {
			row := &group.rows[i]
			if row.finished || row.phase != a.phase || row.label != a.label || row.qualifier != a.qualifier {
				continue
			}
			row.finished, row.elapsed, row.err = true, row.elapsed+a.elapsed, a.err
			// A result follows the subject unless it names it, so the lines of
			// two subjects with the same result stay distinct.
			if a.detail != "" {
				row.detail = a.detail
				if !strings.Contains(a.detail, row.subject) {
					row.detail = row.subject + " (" + a.detail + ")"
				}
			}
			// Keep transfers and slow or failed leaf steps. Enclosing stages
			// and completed project activity do not add another result.
			if group.scope == "" || row.enclosing || row.err == "" && row.elapsed < keptStep && row.unit == "" {
				group.rows = slices.Delete(group.rows, i, i+1)
			}
			break
		}
	}
	if history.Len() > 0 {
		_ = p.paint(time.Now(), history.String())
	}
}

// complete ends a resource's block. A block whose top has scrolled into
// history sends its remaining steps after it, so the list there is whole.
func (p *terminalProgress) complete(scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := slices.IndexFunc(p.groups, func(g *progressGroup) bool { return g.scope == scope })
	if index < 0 {
		return
	}
	history := p.remaining(p.groups[index])
	p.groups = slices.Delete(p.groups, index, index+1)
	if history != "" {
		_ = p.paint(time.Now(), history)
	}
}

// remaining keeps the rest of a partially scrolled block beside its heading.
func (p *terminalProgress) remaining(group *progressGroup) string {
	if !group.scrolled {
		return ""
	}
	width, _ := p.size()
	var rest strings.Builder
	for _, line := range p.resourceLines(group, width, time.Time{}, "", false) {
		rest.WriteString(line.text + "\n")
	}
	return rest.String()
}

// write prints persistent text on its own stream, above the live region, and
// repaints the region in the same call instead of at the next tick.
func (p *terminalProgress) write(out io.Writer, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if out == p.out && !p.stopped && !p.plain {
		return p.paint(time.Now(), text)
	}
	p.clear()
	_, err := io.WriteString(out, text)
	if !p.stopped && !p.plain {
		p.draw(time.Now())
	}
	return err
}

func (p *terminalProgress) stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	var history strings.Builder
	for _, group := range p.groups {
		history.WriteString(p.remaining(group))
	}
	_, _ = io.WriteString(p.out, p.erase()+history.String())
	p.shown, p.last = 0, ""
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

// regionLine is one line of the live region. A block's heading and finished
// steps name their group: they never change, so they can scroll into history.
type regionLine struct {
	text  string
	group *progressGroup
	phase string
	gap   bool
}

func (p *terminalProgress) resourceLines(group *progressGroup, width int, now time.Time, frame string, active bool) []regionLine {
	var lines []regionLine
	if !group.scrolled {
		lines = append(lines, regionLine{text: p.style.heading(runewidth.Truncate(cleanLine(group.scope), max(0, width-3), "…")), group: group})
	}
	phase := group.scrolledPhase
	separated := group.scrolledGap
	var visible []*progressLine
	for i := range group.rows {
		if row := &group.rows[i]; row.finished {
			visible = append(visible, row)
		}
	}
	if running := group.running(); active && running != nil {
		visible = append(visible, running)
	}
	for _, row := range visible {
		indent := stepIndent
		if row.phase != "" {
			if row.phase != phase {
				if phase != "" && !separated {
					lines = append(lines, regionLine{group: group, gap: true})
				}
				title := runewidth.Truncate(cleanLine(row.phase), max(0, width-len(stepIndent)-1), "…")
				lines = append(lines, regionLine{text: stepIndent + p.style.paint(title, color.Faint), group: group, phase: row.phase})
				separated = false
			}
			indent += stepIndent
		}
		phase = row.phase
		line := regionLine{text: progressText(p.style, row, indent, width, now, frame)}
		if row.finished {
			line.group = group
		}
		lines = append(lines, line)
	}
	return lines
}

func (p *terminalProgress) resourceRunning() bool {
	return slices.ContainsFunc(p.groups, func(group *progressGroup) bool {
		return group.scope != "" && group.running() != nil
	})
}

// view keeps completed steps above the active operation. Only a region that
// fits the terminal can be erased, so a taller one
// sends its top lines to history and carries on below them.
func (p *terminalProgress) view(now time.Time, width, height int) (history, region string) {
	frame := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}[(now.UnixMilli()/100)%10]
	var lines, project []regionLine
	resourceRunning := p.resourceRunning()
	for _, group := range p.groups {
		running := group.running()
		if group.scope == "" {
			if running != nil && !resourceRunning {
				project = append(project, regionLine{text: progressText(p.style, running, "", width, now, frame)})
			}
			continue
		}
		lines = append(lines, p.resourceLines(group, width, now, frame, true)...)
	}
	if len(lines) > 0 && len(project) > 0 {
		lines = append(lines, regionLine{})
	}
	lines = append(lines, project...)
	budget := max(0, height-1)
	var scrolled strings.Builder
	for len(lines) > budget && lines[0].group != nil {
		scrolled.WriteString(lines[0].text + "\n")
		lines[0].group.scrollLine(lines[0].phase, lines[0].gap)
		lines = lines[1:]
	}
	if len(lines) > budget {
		lines = lines[len(lines)-budget:]
	}
	texts := make([]string, len(lines))
	for i, line := range lines {
		texts[i] = line.text
	}
	return scrolled.String(), strings.Join(texts, "\n")
}

func (p *terminalProgress) draw(now time.Time) {
	if p.plain {
		resourceRunning := p.resourceRunning()
		for _, group := range p.groups {
			if group.scope == "" && resourceRunning {
				continue
			}
			row := group.running()
			if row == nil || now.Sub(row.started) < 2*time.Second || !row.announced.IsZero() && now.Sub(row.announced) < 30*time.Second {
				continue
			}
			line := cleanLine(row.name())
			if row.phase != "" {
				line = cleanLine(row.phase) + ": " + line
			}
			if group.scope != "" {
				line = cleanLine(group.scope) + ": " + line
			}
			if row.detail != "" {
				line += ": " + cleanLine(row.detail)
			}
			if row.total > 0 && row.unit == "bytes" {
				line += fmt.Sprintf(" (%s / %s)", humanize.IBytes(uint64(max(0, row.current))), humanize.IBytes(uint64(row.total)))
			}
			_, _ = fmt.Fprintf(p.out, "%s (%s)\n", line, now.Sub(row.started).Round(time.Second))
			row.announced = now
		}
		return
	}

	_ = p.paint(now, "")
}

// paint keeps a stderr notice and its redraw in the same terminal write.
func (p *terminalProgress) paint(now time.Time, prefix string) error {
	width, height := p.size()
	history, region := p.view(now, width, height)
	if prefix == "" && history == "" && region == p.last {
		return nil
	}
	_, err := io.WriteString(p.out, p.erase()+prefix+history+region)
	p.last = region
	p.shown = 0
	if region != "" {
		p.shown = strings.Count(region, "\n") + 1
	}
	return err
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

// stepIndent sets a resource's steps under its heading.
const stepIndent = "  "

func progressText(style textStyle, line *progressLine, indent string, width int, now time.Time, frame string) string {
	mark, attribute, duration := frame, color.FgHiCyan, line.elapsed+max(0, now.Sub(line.started))
	if line.finished {
		mark, attribute, duration = "✓", color.FgHiGreen, line.elapsed
		if line.err != "" {
			mark, attribute = "✗", color.FgHiRed
		}
	}
	faint := func(text string) segment { return segment{text, style.paint(text, color.Faint)} }
	var bar, counts segment
	if line.unit != "" {
		counts = faint(amount(line.current, line.unit))
		if !line.finished && line.total > 0 {
			counts = faint(amounts(line.current, line.total, line.unit))
			filled := int(min(barWidth, max(0, barWidth*line.current/line.total)))
			done, left := strings.Repeat("━", filled), strings.Repeat("─", barWidth-filled)
			bar = segment{done + left, style.paint(done, color.FgHiCyan) + style.paint(left, color.Faint)}
		}
	}
	var elapsed segment
	if duration = duration.Round(time.Second); duration > 0 {
		elapsed = faint("(" + duration.String() + ")")
	}
	label, detail := cleanLine(line.name()), cleanLine(line.detail)
	available := max(0, width-runewidth.StringWidth(indent+mark)-2)
	measure := func(parts ...segment) int {
		total := 0
		for i, part := range parts {
			if part.plain != "" {
				separator := 1
				if i == 0 {
					separator = 2 // The detail follows ": "; counters use one space.
				}
				total += separator + runewidth.StringWidth(part.plain)
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
	tail := "…"
	if available-measure(parts...) < runewidth.StringWidth(tail) {
		tail = ""
	}
	var text strings.Builder
	text.WriteString(indent + style.paint(mark, attribute) + " " + runewidth.Truncate(label, max(0, available-measure(parts...)), tail))
	for i, part := range parts {
		if part.plain != "" {
			separator := " "
			if i == 0 {
				separator = ": "
			}
			text.WriteString(separator + part.painted)
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
