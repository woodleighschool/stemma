package main

import (
	"io"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/mattn/go-runewidth"
	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/engine"
	"golang.org/x/term"
)

// textStyle colours terminal output; other writers receive plain text.
type textStyle struct {
	enabled bool
	width   int
}

func isTerminal(out io.Writer) bool {
	if writer, ok := out.(finalWriter); ok {
		out = writer.Writer
	}
	file, ok := out.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func terminalOutput(out io.Writer) bool {
	return isTerminal(out) && os.Getenv("TERM") != "dumb"
}

func newTextStyle(out io.Writer) textStyle {
	width := 100
	if writer, ok := out.(finalWriter); ok {
		out = writer.Writer
	}
	if file, ok := out.(*os.File); ok {
		if columns, _, err := term.GetSize(int(file.Fd())); err == nil && columns > 0 {
			width = columns
		}
	}
	return textStyle{enabled: terminalOutput(out) && os.Getenv("NO_COLOR") == "" && os.Getenv("CI") == "", width: width}
}

func (s textStyle) paint(text string, attributes ...color.Attribute) string {
	if !s.enabled || text == "" || len(attributes) == 0 {
		return text
	}
	style := color.New(attributes...)
	style.EnableColor()
	return style.Sprint(text)
}

func resultHeading(text string, status engine.Status) changes.Line {
	mark, role := "✓", changes.New
	switch status {
	case engine.StatusChanged, engine.StatusUnchanged:
	case engine.StatusFailed, engine.StatusBlocked:
		mark, role = "✗", changes.Failure
	case engine.StatusSkipped:
		mark, role = "–", changes.Old
	}
	return changes.Line{{Role: role, Text: mark}, {Role: changes.Plain, Text: " "}, {Role: changes.Strong, Text: text}}
}

func (s textStyle) columns() int {
	if s.width > 0 {
		return s.width
	}
	return 100
}

func (s textStyle) writeHeading(out *strings.Builder, indent string, label, summary changes.Line) {
	line := append(changes.Line{}, label...)
	if indent != "" {
		for i := range line {
			if line[i].Role == changes.Strong {
				line[i].Role = changes.Plain
			}
		}
	}
	line = append(line, changes.Span{Role: line[len(line)-1].Role, Text: ":"})
	if len(summary) > 0 {
		if runewidth.StringWidth(indent+line.String()+" "+summary.String()) >= s.columns() {
			s.writeLine(out, indent, line)
			s.writeLine(out, indent+"  ", summary)
			return
		}
		line = append(line, changes.Span{Role: changes.Plain, Text: " "})
		line = append(line, summary...)
	}
	s.writeLine(out, indent, line)
}

// writeLine wraps before adding colour, retaining the owning scope on every
// continuation. Long field values move beneath their label.
func (s textStyle) writeLine(out *strings.Builder, indent string, line changes.Line) {
	if len(line) == 0 {
		return
	}
	plain := line.String()
	spaces := len(plain) - len(strings.TrimLeft(plain, " "))
	_, line = splitLine(line, spaces)
	indent += strings.Repeat(" ", spaces)
	if len(line) == 0 {
		out.WriteString(indent + "\n")
		return
	}
	plain = line.String()
	if runewidth.StringWidth(indent+plain) >= s.columns() && line[0].Role == changes.Label && strings.HasSuffix(line[0].Text, ": ") {
		s.writeLine(out, indent, changes.Line{{Role: changes.Label, Text: strings.TrimSuffix(line[0].Text, " ")}})
		if len(line) == 4 && line[1].Role == changes.Old && strings.HasSuffix(line[1].Text, " →") && line[3].Role == changes.New {
			s.writeLine(out, indent+"  ", changes.Line{{Role: changes.Old, Text: "- " + strings.TrimSuffix(line[1].Text, " →")}})
			s.writeLine(out, indent+"  ", changes.Line{{Role: changes.New, Text: "+ " + line[3].Text}})
		} else {
			s.writeLine(out, indent+"  ", line[1:])
		}
		return
	}
	continuation := indent
	if strings.HasPrefix(strings.TrimLeft(plain, " "), "- ") || strings.HasPrefix(strings.TrimLeft(plain, " "), "+ ") {
		continuation += "  "
	}
	for len(line) > 0 {
		plain = line.String()
		end := strings.IndexByte(plain, '\n')
		if end < 0 {
			end = len(plain)
		}
		count := len(runewidth.Truncate(plain[:end], max(1, s.columns()-runewidth.StringWidth(indent)-1), ""))
		skip := 0
		if count < end {
			space := strings.LastIndexByte(plain[:count], ' ')
			slash := strings.LastIndexByte(plain[:count], '/')
			if slash > space && slash > 0 {
				count = slash + 1
			} else if space > 2 {
				count, skip = space, 1
			}
		} else if end < len(plain) {
			skip = 1
		}
		if count == 0 && skip == 0 {
			// A grapheme wider than the remaining column must still be emitted.
			for _, r := range plain {
				count = len(string(r))
				break
			}
		}
		part, rest := splitLine(line, count)
		out.WriteString(indent + s.line(part) + "\n")
		_, line = splitLine(rest, skip)
		indent = continuation
	}
}

// splitLine preserves span roles when a rendered line wraps at a byte boundary.
func splitLine(line changes.Line, count int) (changes.Line, changes.Line) {
	var before changes.Line
	for i, span := range line {
		if count < len(span.Text) {
			if count > 0 {
				before = append(before, changes.Span{Role: span.Role, Text: span.Text[:count]})
			}
			after := append(changes.Line{{Role: span.Role, Text: span.Text[count:]}}, line[i+1:]...)
			return before, after
		}
		before = append(before, span)
		count -= len(span.Text)
	}
	return before, nil
}

// roleColors give each role of a row's spans its meaning in colour; the text
// reads the same without it.
var roleColors = map[changes.Role][]color.Attribute{
	changes.Plain:   nil,
	changes.Old:     {color.Faint},
	changes.New:     {color.FgHiGreen},
	changes.Notable: {color.FgHiYellow},
	changes.Failure: {color.FgHiRed},
	changes.Blocked: {color.FgHiYellow},
	changes.Label:   nil,
	changes.Added:   {color.FgGreen},
	changes.Removed: {color.FgRed},
	changes.Strong:  {color.Bold},
}

func (s textStyle) line(line changes.Line) string {
	var text strings.Builder
	for _, span := range line {
		text.WriteString(s.paint(span.Text, roleColors[span.Role]...))
	}
	return text.String()
}

func printResult(out io.Writer, subject, summary string, fields ...[2]string) error {
	row := changes.Row{Summary: changes.Line{{Role: changes.New, Text: changes.Text(summary)}}}
	for _, field := range fields {
		row.Details = append(row.Details, changes.Line{{Role: changes.Label, Text: field[0] + ": "}, {Role: changes.Plain, Text: changes.Text(field[1])}})
	}
	_, err := io.WriteString(out, renderRows(newTextStyle(out), subject, engine.StatusChanged, []changes.Row{row}))
	return err
}
