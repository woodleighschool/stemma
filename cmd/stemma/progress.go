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
	// JSON plugin records carry numeric attributes as float64.
	if value.Kind() == slog.KindFloat64 {
		return int64(value.Float64())
	}
	return 0
}

type progressLine struct {
	activity

	started, ended time.Time
	outcome        string
	heading        bool
}

type progressGroup struct {
	scope string
	rows  []*progressLine
}

// Completed groups become ordinary scrollback. Only unfinished groups remain
// under terminal control, including their finished operation rows.
type terminalProgress struct {
	progress *tea.Program
	out      io.Writer
	style    textStyle
	groups   []*progressGroup
}

func newTerminalProgress(out io.Writer) *terminalProgress {
	return &terminalProgress{out: out, style: newTextStyle(out)}
}

func (p *terminalProgress) group(scope string) *progressGroup {
	for _, group := range p.groups {
		if group.scope == scope {
			return group
		}
	}
	group := &progressGroup{scope: scope}
	p.groups = append(p.groups, group)
	label := scope
	if label == "" {
		label = "Project"
	}
	p.add(group, progressLine{label: label, detail: "working", started: time.Now(), heading: true})
	return group
}

func (p *terminalProgress) add(group *progressGroup, line progressLine) {
	group.rows = append(group.rows, &line)
	p.show()
}

func (p *terminalProgress) update(a activity) {
	group := p.group(a.scope)
	if a.stage {
		p.add(group, progressLine{activity: a, started: time.Now()})
		return
	}
	for _, row := range slices.Backward(group.rows[1:]) {
		line := *row
		if !line.ended.IsZero() || a.status && line.label != a.label {
			continue
		}
		switch {
		case a.progress:
			line.current, line.total, line.unit = a.current, a.total, a.unit
		case a.status:
			line.ended, line.outcome, line.detail = time.Now(), "done", a.detail
			if a.elapsed > 0 {
				line.ended = line.started.Add(a.elapsed)
			}
			if a.err != "" {
				line.outcome, line.err = "failed", a.err
			}
		default:
			return
		}
		*row = line
		if a.scope == "" && a.status && a.err == "" {
			pending := slices.ContainsFunc(group.rows[1:], func(row *progressLine) bool { return row.ended.IsZero() })
			if !pending {
				p.groups = slices.DeleteFunc(p.groups, func(group *progressGroup) bool { return group.scope == "" })
			}
		}
		p.show()
		return
	}
}

func (p *terminalProgress) note(scope, message, outcome string) {
	p.add(p.group(scope), progressLine{label: message, started: time.Now(), ended: time.Now(), outcome: outcome})
}

func (p *terminalProgress) complete(scope, status string, failed bool) {
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
		for _, row := range group.rows {
			line := *row
			if line.heading {
				line.detail, line.outcome, line.ended = status, "done", now
				if failed {
					line.outcome = "failed"
				}
			} else if line.ended.IsZero() {
				line.ended, line.outcome = now, "unfinished"
			}
			if collapse && !line.heading && line.outcome != "detail" {
				continue
			}
			_, _ = fmt.Fprintln(&result, progressText(p.style, &line, width, now, ""))
			if line.err != "" {
				_, _ = fmt.Fprintln(&result, "      "+cleanLine(line.err))
			}
		}
		p.groups = slices.Delete(p.groups, index, index+1)
		p.show()
		_, _ = p.Write([]byte(result.String()))
		return
	}
}

func (p *terminalProgress) stop(outcome string) {
	for len(p.groups) > 0 {
		status := outcome
		if status == "" {
			status = "finished"
		}
		p.complete(p.groups[0].scope, status, outcome != "")
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
	groups := make(progressSnapshot, len(p.groups))
	for index, group := range p.groups {
		for _, row := range group.rows {
			groups[index] = append(groups[index], *row)
		}
	}
	p.progress.Send(groups)
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
	end := line.ended
	if end.IsZero() {
		end = now
	}
	elapsed := end.Sub(line.started).Round(time.Second)
	if elapsed > 0 || line.ended.IsZero() {
		detail = strings.TrimSpace(detail + " (" + elapsed.String() + ")")
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
	label := runewidth.Truncate(cleanLine(line.label), labelWidth, tail)
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
