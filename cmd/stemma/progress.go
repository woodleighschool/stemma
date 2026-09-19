package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

type activity struct {
	stage, progress, status, final bool
	label, scope, detail, unit     string
	current, total                 int64
	elapsed                        time.Duration
	err                            string
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
		case "progress_final":
			a.final = attr.Value.Kind() == slog.KindBool && attr.Value.Bool()
		case "current":
			a.current = activityCount(attr.Value)
		case "total":
			a.total = activityCount(attr.Value)
		case "elapsed":
			a.elapsed = time.Duration(activityCount(attr.Value))
		case "unit":
			a.unit = attr.Value.String()
		case "error":
			if attr.Value.Any() != nil {
				a.err = attr.Value.String()
			}
		case "resource", "input", "destination", "plugin", "cached":
			values[attr.Key] = attr.Value.String()
		}
		return true
	}
	for _, attr := range attrs {
		read(attr)
	}
	record.Attrs(read)
	a.scope = values["resource"]
	for _, key := range []string{"input", "destination", "plugin"} {
		if values[key] != "" {
			a.label += " (" + values[key] + ")"
		}
	}
	if values["cached"] == "true" {
		a.detail = "cached"
	}
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

type progressLine struct {
	activity

	started, ended time.Time
	outcome        string
	heading        bool
	// Stages that start while this operation is open are its steps, innermost
	// last. The row shows the current step until the operation finishes.
	steps []progressStep
}

type progressStep struct {
	label   string
	started time.Time
}

// finish ends the innermost step or the operation named by a stage result.
func (row *progressLine) finish(a activity) bool {
	for index, step := range slices.Backward(row.steps) {
		if step.label == a.label {
			row.steps = slices.Delete(row.steps, index, index+1)
			return true
		}
	}
	if row.label != a.label {
		return false
	}
	row.ended, row.outcome, row.detail, row.steps = time.Now(), "done", a.detail, nil
	if a.elapsed > 0 {
		row.ended = row.started.Add(a.elapsed)
	}
	if a.err != "" {
		row.outcome, row.err = "failed", a.err
	}
	return true
}

type progressGroup struct {
	scope string
	rows  []*progressLine
}

// open returns the unfinished operation. Nested stages become its steps, so a
// group has at most one.
func (g *progressGroup) open() *progressLine {
	for _, row := range slices.Backward(g.rows[1:]) {
		if row.ended.IsZero() {
			return row
		}
	}
	return nil
}

// Completed groups become ordinary scrollback. Only unfinished groups remain
// under terminal control, including their finished operation rows.
type terminalProgress struct {
	progress *tea.Program
	out      io.Writer
	style    textStyle
	groups   []*progressGroup
	recent   string
}

func newTerminalProgress(out io.Writer) *terminalProgress {
	return &terminalProgress{out: out, style: newTextStyle(out)}
}

func (p *terminalProgress) find(scope string) *progressGroup {
	for _, group := range p.groups {
		if group.scope == scope {
			return group
		}
	}
	return nil
}

func (p *terminalProgress) group(scope string) *progressGroup {
	if group := p.find(scope); group != nil {
		return group
	}
	label := scope
	if label == "" {
		label = "Project"
	}
	group := &progressGroup{scope: scope, rows: []*progressLine{{label: label, heading: true}}}
	p.groups = append(p.groups, group)
	return group
}

func (p *terminalProgress) update(a activity) {
	group := p.find(a.scope)
	if a.stage {
		group = p.group(a.scope)
	}
	if group == nil {
		return
	}
	p.recent = a.scope
	row := group.open()
	switch {
	case a.stage && row != nil:
		row.steps = append(row.steps, progressStep{label: a.label, started: time.Now()})
		row.current, row.total, row.unit = a.current, a.total, a.unit
	case a.stage:
		group.rows = append(group.rows, &progressLine{activity: a, started: time.Now()})
	case row == nil:
		return
	case a.progress:
		row.current, row.total, row.unit = a.current, a.total, a.unit
	case a.status:
		if !row.finish(a) {
			return
		}
		// Successful project setup disappears; resource trees keep their results.
		if a.scope == "" && !slices.ContainsFunc(group.rows[1:], func(row *progressLine) bool { return row.outcome != "done" }) {
			p.groups = slices.DeleteFunc(p.groups, func(other *progressGroup) bool { return other == group })
		}
	default:
		return
	}
	p.show()
}

func (p *terminalProgress) note(scope, message, outcome string) {
	group := p.group(scope)
	now := time.Now()
	group.rows = append(group.rows, &progressLine{label: message, started: now, ended: now, outcome: outcome})
	p.recent = scope
	p.show()
}

// complete moves a group to scrollback and reports whether it had one. Rows
// mark the stage that failed; the failure text belongs to the group.
func (p *terminalProgress) complete(scope, status string, failed bool, failure []string) bool {
	for index, group := range p.groups {
		if group.scope != scope {
			continue
		}
		var result strings.Builder
		now := time.Now()
		width, _ := p.size()
		collapse := !failed && !slices.ContainsFunc(group.rows[1:], func(line *progressLine) bool {
			return line.ended.IsZero() || line.outcome == "failed" || line.outcome == "warning"
		})
		// Groups can wait between phases, so headings report time spent working.
		var active time.Duration
		for _, row := range group.rows[1:] {
			end := row.ended
			if end.IsZero() {
				end = now
			}
			active += end.Sub(row.started)
		}
		for _, row := range group.rows {
			line := *row
			if line.heading {
				line.detail, line.outcome, line.started, line.ended = status, "done", now.Add(-active), now
				if failed {
					line.outcome = "failed"
				}
			} else if line.ended.IsZero() {
				line.ended, line.outcome, line.steps = now, "unfinished", nil
			}
			if collapse && !line.heading && line.outcome != "detail" {
				continue
			}
			_, _ = fmt.Fprintln(&result, progressText(p.style, &line, width, now, ""))
		}
		for _, line := range failure {
			_, _ = fmt.Fprintln(&result, "      "+line)
		}
		p.groups = slices.Delete(p.groups, index, index+1)
		p.show()
		_, _ = p.Write([]byte(result.String()))
		return true
	}
	return false
}

func (p *terminalProgress) stop(outcome string) {
	for len(p.groups) > 0 {
		status := outcome
		if status == "" {
			status = "finished"
		}
		p.complete(p.groups[0].scope, status, outcome != "", nil)
	}
	if p.progress != nil {
		p.progress.Quit()
		p.progress.Wait()
		p.progress = nil
	}
}

func (p *terminalProgress) Write(data []byte) (int, error) {
	if p.progress != nil {
		p.progress.Send(tea.Println(strings.TrimSuffix(string(data), "\n"))())
		return len(data), nil
	}
	return p.out.Write(data)
}

func (p *terminalProgress) size() (int, int) {
	if file, ok := p.out.(*os.File); ok {
		if width, height, err := term.GetSize(int(file.Fd())); err == nil && width > 0 && height > 0 {
			return width, height
		}
	}
	return 100, 28
}

// Send immutable snapshots to the renderer; operation ownership stays here.
func (p *terminalProgress) show() {
	if !terminalOutput(p.out) {
		return
	}
	if p.progress == nil {
		width, height := p.size()
		model := progressModel{style: p.style, width: width, height: height, spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot))}
		program := tea.NewProgram(model, tea.WithOutput(p.out), tea.WithInput(nil), tea.WithoutSignalHandler(), tea.WithWindowSize(width, height))
		p.progress = program
		go func() { _, _ = program.Run() }()
	}
	p.progress.Send(p.snapshot())
}

// snapshot copies groups with an open operation. Idle groups wait off screen
// until they complete, except the latest one, so gaps between stages do not
// blank the display.
func (p *terminalProgress) snapshot() progressSnapshot {
	var groups progressSnapshot
	for _, group := range p.groups {
		if group.open() == nil && group.scope != p.recent {
			continue
		}
		rows := make([]progressLine, 0, len(group.rows))
		for _, row := range group.rows {
			line := *row
			line.steps = slices.Clone(row.steps)
			rows = append(rows, line)
		}
		groups = append(groups, rows)
	}
	return groups
}

type progressSnapshot [][]progressLine

type progressModel struct {
	groups        progressSnapshot
	style         textStyle
	width, height int
	spinner       spinner.Model
}

func (m progressModel) Init() tea.Cmd { return m.spinner.Tick }

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressSnapshot:
		m.groups = msg
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m progressModel) View() tea.View {
	var lines []string
	remaining := max(1, m.height-1)
	for index, group := range m.groups {
		if remaining == 0 {
			break
		}
		// Share the available rows between active groups, preserving their
		// headings and latest operations. Final results retain every failure.
		budget := max(1, remaining/(len(m.groups)-index))
		for row, line := range group {
			if row != 0 && row <= len(group)-budget {
				continue
			}
			lines = append(lines, progressText(m.style, &line, m.width, time.Now(), m.spinner.View()))
			remaining--
		}
	}
	return tea.NewView(strings.Join(lines, "\n"))
}

func progressText(style textStyle, line *progressLine, width int, now time.Time, frame string) string {
	prefix := "    "
	var mark string
	var attribute color.Attribute
	switch line.outcome {
	case "done":
		mark, attribute = "✓ ", color.FgHiGreen
	case "failed":
		mark, attribute = "✗ ", color.FgHiRed
	case "detail":
		mark, attribute = "- ", color.Faint
	case "warning", "unfinished":
		mark, attribute = "! ", color.FgHiYellow
	default:
		mark, attribute = frame+" ", color.FgHiCyan
	}
	if line.heading {
		prefix = ""
		if line.outcome == "" {
			mark = ""
		}
	}
	label, started := line.label, line.started
	if len(line.steps) > 0 {
		step := line.steps[len(line.steps)-1]
		label, started = step.label, step.started
	}
	detail := line.detail
	if line.unit != "" {
		count := strconv.FormatInt(line.current, 10)
		if line.unit == "bytes" {
			count = humanize.IBytes(uint64(max(0, line.current)))
		}
		if line.total > 0 {
			if line.unit == "bytes" {
				count += " / " + humanize.IBytes(uint64(line.total))
			} else {
				count += fmt.Sprintf("/%d", line.total)
			}
		}
		if line.unit != "bytes" {
			count += " " + line.unit
		}
		if line.total > 0 {
			count += fmt.Sprintf(" %.0f%%", 100*float64(line.current)/float64(line.total))
		}
		detail = strings.TrimSpace(detail + " " + count)
	}
	// Live headings only name their group; operation rows carry the timing.
	if !line.heading || !line.ended.IsZero() {
		end := line.ended
		if end.IsZero() {
			end = now
		}
		elapsed := end.Sub(started).Round(time.Second)
		if elapsed > 0 || line.ended.IsZero() {
			detail = strings.TrimSpace(detail + " (" + elapsed.String() + ")")
		}
	}
	if line.outcome == "unfinished" {
		detail = strings.TrimSpace(detail + " not completed")
	}
	suffix := ""
	if detail != "" {
		suffix = "  " + detail
	}
	available := max(0, width-runewidth.StringWidth(prefix+mark)-1)
	suffix = runewidth.Truncate(cleanLine(suffix), available, "")
	labelWidth := max(0, available-runewidth.StringWidth(suffix))
	tail := "..."
	if labelWidth < len(tail) {
		tail = ""
	}
	label = runewidth.Truncate(cleanLine(label), labelWidth, tail)
	if line.heading {
		label = style.paint(label, color.Bold)
	}
	return prefix + style.paint(mark, attribute) + label + style.paint(suffix, color.Faint)
}

func cleanLine(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
