package main

import (
	"io"
	"log/slog"
	"os"
	"slices"
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

type progressLine struct {
	activity

	// parent is the row of the enclosing stage; 0 is the group's heading.
	parent, depth  int
	started, ended time.Time
	outcome        string
	heading        bool
}

func (row *progressLine) open() bool { return !row.heading && row.ended.IsZero() }

type progressGroup struct {
	scope string
	rows  []*progressLine
}

// innermost returns the latest unfinished stage, or 0 when none is open.
// Stages nest, so it encloses whatever starts next.
func (g *progressGroup) innermost() int {
	for index := len(g.rows) - 1; index > 0; index-- {
		if g.rows[index].open() {
			return index
		}
	}
	return 0
}

// finish ends the innermost open stage the result names, and any stage still
// open inside it. A result's detail replaces the stage's subject.
func (g *progressGroup) finish(a activity) bool {
	for index := len(g.rows) - 1; index > 0; index-- {
		row := g.rows[index]
		if !row.open() || row.label != a.label || row.qualifier != a.qualifier {
			continue
		}
		now := time.Now()
		row.ended, row.outcome = now, "done"
		if a.elapsed > 0 {
			row.ended = row.started.Add(a.elapsed)
		}
		if a.detail != "" {
			row.detail = a.detail
		}
		if a.err != "" {
			row.outcome, row.err = "failed", a.err
		}
		for _, inner := range g.rows[index+1:] {
			if inner.open() {
				inner.ended, inner.outcome = now, row.outcome
			}
		}
		return true
	}
	return false
}

// terminalProgress keeps a live tree of unfinished work below persistent
// output: one group of stage rows per resource, and one for project work. A
// finished resource leaves the tree; its report takes its place above.
type terminalProgress struct {
	program *tea.Program
	out     io.Writer
	style   textStyle
	groups  []*progressGroup
	recent  string
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
	switch {
	case a.stage:
		parent := group.innermost()
		group.rows = append(group.rows, &progressLine{activity: a, parent: parent, depth: group.rows[parent].depth + 1, started: time.Now()})
	case a.progress:
		index := group.innermost()
		if index == 0 {
			return
		}
		row := group.rows[index]
		row.current, row.total, row.unit = a.current, a.total, a.unit
	case a.status:
		if !group.finish(a) {
			return
		}
		// Successful project work leaves the tree; a failed stage stays until
		// the command reports its error.
		if a.scope == "" && !slices.ContainsFunc(group.rows[1:], func(row *progressLine) bool { return row.outcome != "done" }) {
			p.groups = slices.DeleteFunc(p.groups, func(other *progressGroup) bool { return other == group })
		}
	default:
		return
	}
	p.show()
}

// complete removes a finished resource from the tree.
func (p *terminalProgress) complete(scope string) {
	p.groups = slices.DeleteFunc(p.groups, func(group *progressGroup) bool { return group.scope == scope })
	p.show()
}

// running reports whether the tree is on screen, so persistent output must
// be printed above it.
func (p *terminalProgress) running() bool { return p.program != nil }

// print writes persistent output above the tree.
func (p *terminalProgress) print(text string) {
	p.program.Send(tea.Println(strings.TrimSuffix(text, "\n"))())
}

// stop clears the tree and waits for the renderer to release the terminal.
func (p *terminalProgress) stop() {
	p.groups = nil
	if p.program != nil {
		p.program.Send(stopProgress{})
		p.program.Wait()
		p.program = nil
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

// Send immutable snapshots to the renderer; operation ownership stays here.
func (p *terminalProgress) show() {
	if !terminalOutput(p.out) {
		return
	}
	if p.program == nil {
		width, height := p.size()
		model := progressModel{style: p.style, width: width, height: height, spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot))}
		program := tea.NewProgram(model, tea.WithOutput(p.out), tea.WithInput(nil), tea.WithoutSignalHandler(), tea.WithWindowSize(width, height))
		p.program = program
		go func() { _, _ = program.Run() }()
	}
	p.program.Send(p.snapshot())
}

// snapshot copies the visible rows of groups with unfinished work, and of the
// latest group so gaps between stages do not blank the display. A finished
// stage folds the steps it enclosed into its own row, and a step names what it
// works for only when its stage does not.
func (p *terminalProgress) snapshot() progressSnapshot {
	var groups progressSnapshot
	for _, group := range p.groups {
		if group.innermost() == 0 && group.scope != p.recent {
			continue
		}
		visible := make([]bool, len(group.rows))
		var rows []progressLine
		for index, row := range group.rows {
			parent := group.rows[row.parent]
			visible[index] = index == 0 || visible[row.parent] && (row.parent == 0 || parent.open())
			if !visible[index] {
				continue
			}
			line := *row
			if row.parent != 0 && line.qualifier == parent.qualifier {
				line.qualifier = ""
			}
			rows = append(rows, line)
		}
		groups = append(groups, rows)
	}
	return groups
}

type progressSnapshot [][]progressLine

type stopProgress struct{}

type progressModel struct {
	groups        progressSnapshot
	style         textStyle
	width, height int
	spinner       spinner.Model
	stopped       bool
}

func (m progressModel) Init() tea.Cmd { return m.spinner.Tick }

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressSnapshot:
		m.groups = msg
	case stopProgress:
		m.groups, m.stopped = nil, true
		return m, tea.Quit
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m progressModel) View() tea.View {
	if m.stopped {
		return tea.NewView("")
	}
	var lines []string
	remaining := max(1, m.height-1)
	now := time.Now()
	for index, group := range m.groups {
		if remaining == 0 {
			break
		}
		// Share the rows between groups, so each keeps its heading and its
		// unfinished stages.
		budget := max(1, remaining/(len(m.groups)-index))
		for _, line := range fit(group, budget) {
			lines = append(lines, progressText(m.style, &line, m.width, now, m.spinner.View()))
			remaining--
		}
	}
	return tea.NewView(strings.Join(lines, "\n"))
}

// fit keeps a group's heading and latest rows within budget, dropping
// finished stages before unfinished ones.
func fit(group []progressLine, budget int) []progressLine {
	if len(group) <= budget {
		return group
	}
	rows := slices.Clone(group)
	for index := 1; len(rows) > budget && index < len(rows); {
		if rows[index].open() {
			index++
			continue
		}
		rows = slices.Delete(rows, index, index+1)
	}
	if len(rows) > budget {
		rows = append(rows[:1], rows[len(rows)-budget+1:]...)
	}
	return rows
}

// barWidth is the width of a transfer's bar, which is dropped first when a
// row does not fit.
const barWidth = 20

// segment is rendered text with the plain text that sets its width.
type segment struct{ plain, painted string }

func progressText(style textStyle, line *progressLine, width int, now time.Time, frame string) string {
	if line.heading {
		return style.paint(runewidth.Truncate(cleanLine(line.label), max(0, width-1), "..."), color.Bold)
	}
	var mark string
	var attribute color.Attribute
	switch line.outcome {
	case "done":
		mark, attribute = "✓", color.FgHiGreen
	case "failed":
		mark, attribute = "✗", color.FgHiRed
	default:
		mark, attribute = frame, color.FgHiCyan
	}
	// A step's mark sits under its stage's label.
	indent := strings.Repeat(" ", 2+2*line.depth)
	faint := func(text string) segment { return segment{text, style.paint(text, color.Faint)} }
	var bar, counts segment
	if line.unit != "" {
		counts = faint(amount(line.current, line.unit))
		if line.open() && line.total > 0 {
			counts = faint(amounts(line.current, line.total, line.unit))
			filled := int(min(barWidth, max(0, barWidth*line.current/line.total)))
			done, left := strings.Repeat("━", filled), strings.Repeat("─", barWidth-filled)
			bar = segment{done + left, style.paint(done, color.FgHiCyan) + style.paint(left, color.Faint)}
		}
	}
	end := line.ended
	if end.IsZero() {
		end = now
	}
	var elapsed segment
	if duration := end.Sub(line.started).Round(time.Second); duration > 0 || line.open() {
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
