package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/mattn/go-runewidth"
)

type activity struct {
	stage, progress, status bool
	// label is the stage message; qualifier names the input, destination or
	// plugin it works for.
	label, qualifier, scope, phase, item, detail, unit string
	current, total                                     int64
	elapsed                                            time.Duration
	err                                                string
}

// name is the stage message with what it works for.
func (a activity) name() string {
	if a.qualifier == "" || a.qualifier == "source" {
		return a.label
	}
	return a.qualifier + ": " + a.label
}

func (a activity) description() string {
	if a.item == "" {
		return a.detail
	}
	if a.detail == "" {
		return a.item
	}
	return a.item + " (" + a.detail + ")"
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
		case "item":
			a.item = attr.Value.String()
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

	started, announced time.Time
	enclosing          bool
}

type progressGroup struct {
	scope string
	rows  []progressLine
}

func (g *progressGroup) running() *progressLine {
	for i := len(g.rows) - 1; i >= 0; i-- {
		if !g.rows[i].status {
			return &g.rows[i]
		}
	}
	return nil
}

// progressState holds the operations beneath each unfinished resource.
type progressState struct {
	style  textStyle
	groups []*progressGroup
}

func (p *progressState) update(a activity) {
	index := slices.IndexFunc(p.groups, func(g *progressGroup) bool { return g.scope == a.scope })
	if index < 0 {
		if !a.stage {
			return
		}
		index = len(p.groups)
		p.groups = append(p.groups, &progressGroup{scope: a.scope})
	}
	group := p.groups[index]
	if a.stage {
		for i := range group.rows {
			row := &group.rows[i]
			if !row.status && row.phase == a.phase && (row.qualifier == a.qualifier || row.qualifier == "") {
				row.enclosing = true
			}
		}
		group.rows = append(group.rows, progressLine{activity: a, started: time.Now()})
		return
	}
	for i := len(group.rows) - 1; i >= 0; i-- {
		row := &group.rows[i]
		if row.status || row.phase != a.phase || row.qualifier != a.qualifier {
			continue
		}
		if a.progress {
			row.current, row.total, row.unit = a.current, a.total, a.unit
			break
		}
		if a.status && row.label == a.label {
			row.status, row.err, row.elapsed = true, a.err, a.elapsed
			if a.detail != "" {
				row.detail = a.detail
			}
			if group.scope == "" {
				group.rows = slices.Delete(group.rows, i, i+1)
			}
			break
		}
	}
	if len(group.rows) == 0 {
		p.groups = slices.Delete(p.groups, index, index+1)
	}
}

func (p *progressState) complete(scope string) {
	p.groups = slices.DeleteFunc(p.groups, func(g *progressGroup) bool { return g.scope == scope })
}

func (p *progressState) resourceRunning() bool {
	return slices.ContainsFunc(p.groups, func(group *progressGroup) bool { return group.scope != "" && group.running() != nil })
}

func (p *progressState) view(now time.Time, width, height int) string {
	frame := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}[(now.UnixMilli()/100)%10]
	type line struct {
		text      string
		completed bool
		heading   bool
		indent    int
	}
	var lines []line
	resourceRunning := p.resourceRunning()
	for _, group := range p.groups {
		if group.running() == nil || group.scope == "" && resourceRunning {
			continue
		}
		indent := ""
		if group.scope != "" {
			heading := runewidth.Truncate(cleanLine(group.scope)+":", max(0, width-1), "…")
			lines = append(lines, line{text: p.style.paint(heading, color.Bold, color.FgHiMagenta), heading: true})
			indent = "  "
		}
		var rows []progressLine
		for i, row := range group.rows {
			if row.status && row.enclosing {
				continue
			}
			if !row.status && slices.ContainsFunc(group.rows[i+1:], func(child progressLine) bool {
				return !child.status && child.phase == row.phase && (child.qualifier == row.qualifier || row.qualifier == "")
			}) {
				continue
			}
			rows = append(rows, row)
		}
		owner := ""
		for i, row := range rows {
			stepIndent := indent
			qualifier := row.qualifier
			if qualifier == "source" {
				qualifier = ""
			}
			if qualifier != "" {
				if qualifier != owner {
					heading := runewidth.Truncate(indent+cleanLine(qualifier)+":", max(0, width-1), "…")
					lines = append(lines, line{text: heading, heading: true, indent: len(indent)})
				}
				stepIndent += "  "
			}
			owner = qualifier
			if row.item != "" {
				if i == 0 || rows[i-1].item == "" || rows[i-1].label != row.label || rows[i-1].qualifier != row.qualifier || rows[i-1].phase != row.phase {
					heading := runewidth.Truncate(stepIndent+cleanLine(row.label)+":", max(0, width-1), "…")
					lines = append(lines, line{text: heading, heading: true, indent: len(stepIndent)})
				}
				stepIndent += "  "
			}
			for _, text := range progressText(p.style, &row, stepIndent, width, now, frame) {
				lines = append(lines, line{text: text, completed: row.status, indent: len(stepIndent)})
			}
		}
	}
	budget := max(0, height-1)
	for len(lines) > budget {
		index := slices.IndexFunc(lines, func(l line) bool { return l.completed })
		if index < 0 {
			return p.compact(now, width, budget, frame)
		}
		lines = slices.Delete(lines, index, index+1)
		for i, l := range slices.Backward(lines) {
			if l.heading && (i+1 == len(lines) || lines[i+1].indent <= l.indent) {
				lines = slices.Delete(lines, i, i+1)
			}
		}
	}
	text := make([]string, len(lines))
	for i, line := range lines {
		text[i] = line.text
	}
	return strings.Join(text, "\n")
}

// compact keeps the owner beside each active operation when the expanded
// display cannot fit. A bar is useful only after its identity fits.
func (p *progressState) compact(now time.Time, width, budget int, frame string) string {
	if budget == 0 {
		return ""
	}
	var groups []*progressGroup
	for _, group := range p.groups {
		if group.running() != nil && (group.scope != "" || !p.resourceRunning()) {
			groups = append(groups, group)
		}
	}
	var lines []string
	limit := budget
	if len(groups) > budget && budget > 1 {
		limit--
	}
	bars := budget - min(len(groups), limit)
	if len(groups) > limit && budget > 1 {
		bars--
	}
	for _, group := range groups[:min(len(groups), limit)] {
		row := group.running()
		label := frame + " " + activityLabel(group.scope, row.activity)
		if budget == 1 && len(groups) > 1 {
			label += fmt.Sprintf(" (+%d active)", len(groups)-1)
		}
		label = runewidth.Truncate(label, max(0, width-1), "…")
		title := cleanLine(group.scope) + ":"
		if suffix, ok := strings.CutPrefix(label, frame+" "+title); ok && group.scope != "" {
			label = frame + " " + p.style.paint(title, color.Bold, color.FgHiMagenta) + suffix
		}
		lines = append(lines, label)
		if bars > 0 {
			step := progressText(p.style, row, "", width, now, frame)
			if len(step) > 1 {
				lines = append(lines, step[1])
				bars--
			}
		}
	}
	if len(groups) > limit && budget > 1 {
		lines = append(lines, fmt.Sprintf("%d more active resources", len(groups)-limit))
	}
	return strings.Join(lines, "\n")
}

func activityLabel(scope string, a activity) string {
	label := a.name()
	if description := a.description(); description != "" {
		label += ": " + description
	}
	if scope != "" {
		label = scope + ": " + label
	}
	return cleanLine(label)
}

func (p *progressState) plain(now time.Time) string {
	var text strings.Builder
	resourceRunning := p.resourceRunning()
	for _, group := range p.groups {
		if group.scope == "" && resourceRunning {
			continue
		}
		row := group.running()
		if row == nil || now.Sub(row.started) < 2*time.Second || !row.announced.IsZero() && now.Sub(row.announced) < 30*time.Second {
			continue
		}
		line := activityLabel(group.scope, row.activity)
		if row.total > 0 && row.unit == "bytes" {
			line += fmt.Sprintf(" (%s / %s)", humanize.IBytes(uint64(max(0, row.current))), humanize.IBytes(uint64(row.total)))
		}
		_, _ = fmt.Fprintf(&text, "%s (%s)\n", line, now.Sub(row.started).Round(time.Second))
		row.announced = now
	}
	return text.String()
}

func progressText(style textStyle, line *progressLine, indent string, width int, now time.Time, frame string) []string {
	mark, duration := frame, max(0, now.Sub(line.started))
	var attributes []color.Attribute
	if line.status {
		mark, duration = "✓", line.elapsed
		attributes = []color.Attribute{color.Faint}
		if line.err != "" {
			mark, attributes = "✗", []color.Attribute{color.FgHiRed}
		}
	}
	label := cleanLine(line.label)
	if line.item != "" {
		label = cleanLine(line.description())
	} else if line.detail != "" {
		label += ": " + cleanLine(line.detail)
	}
	if elapsed := duration.Round(time.Second); elapsed > 0 {
		label += " (" + elapsed.String() + ")"
	}
	prefix := indent + mark + " "
	label = runewidth.Truncate(label, max(0, width-runewidth.StringWidth(prefix)-1), "…")
	text := indent + style.paint(mark, attributes...) + " " + label
	if runewidth.StringWidth(prefix) >= width {
		text = runewidth.Truncate(prefix, max(0, width-1), "…")
	}
	lines := []string{text}
	if line.unit == "" || line.status {
		return lines
	}
	counts := amount(line.current, line.unit)
	if line.total > 0 {
		counts = amounts(line.current, line.total, line.unit)
		counts += fmt.Sprintf(" %d%%", min(100, max(0, 100*line.current/line.total)))
	}
	barIndent := indent + "  "
	available := max(0, width-runewidth.StringWidth(barIndent)-2)
	barWidth := min(20, available)
	if available-runewidth.StringWidth(counts)-1 >= 5 {
		barWidth = min(barWidth, available-runewidth.StringWidth(counts)-1)
	} else {
		counts = ""
	}
	bar := make([]rune, barWidth)
	for i := range bar {
		bar[i] = '─'
		if line.total > 0 {
			if int64(i) < int64(barWidth)*max(0, min(line.current, line.total))/line.total {
				bar[i] = '━'
			}
		} else if barWidth > 0 && (i-int(now.UnixMilli()/100)%barWidth+barWidth)%barWidth < min(3, barWidth) {
			bar[i] = '━'
		}
	}
	if counts != "" {
		counts = " " + counts
	}
	lines = append(lines, runewidth.Truncate(barIndent+string(bar)+counts, max(0, width-1), "…"))
	return lines
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
