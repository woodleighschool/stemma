package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/fatih/color"
	"github.com/woodleighschool/stemma/internal/changes"
	"golang.org/x/term"
)

// textStyle colours terminal output; other writers receive plain text.
type textStyle struct {
	enabled bool
}

func terminalOutput(out io.Writer) bool {
	if writer, ok := out.(finalWriter); ok {
		out = writer.Writer
	}
	file, ok := out.(*os.File)
	return ok && term.IsTerminal(int(file.Fd())) && os.Getenv("TERM") != "dumb"
}

func newTextStyle(out io.Writer) textStyle {
	return textStyle{enabled: terminalOutput(out) && os.Getenv("NO_COLOR") == "" && os.Getenv("CI") == ""}
}

func (s textStyle) paint(text string, attributes ...color.Attribute) string {
	if !s.enabled || text == "" {
		return text
	}
	style := color.New(attributes...)
	style.EnableColor()
	return style.Sprint(text)
}

// heading marks a section or a resource's live or detailed block.
func (s textStyle) heading(text string) string {
	return s.paint("➤ "+text, color.Bold, color.FgHiMagenta)
}

// outcome colours a status by what it means for the reader.
func (s textStyle) outcome(text string) string {
	_, attribute := outcomeStyle(text)
	return s.paint(text, attribute)
}

func outcomeStyle(text string) (string, color.Attribute) {
	switch {
	case strings.HasPrefix(text, "failed"):
		return "✗", color.FgHiRed
	case slices.Contains([]string{"blocked", "skipped", "declined"}, text):
		return "–", color.FgHiYellow
	case strings.HasPrefix(text, "no application selected"), slices.Contains([]string{"no installer output", "no artwork", "no icon declared", "nothing to declare"}, text):
		return "–", color.Faint
	case slices.Contains([]string{"unchanged", "inputs unchanged", "already applied", "cached", "pinned", "retired"}, text):
		return "✓", color.Faint
	}
	return "✓", color.FgHiGreen
}

// Failed blocks put the cross on the cause beneath their heading.
func (s textStyle) outcomeLine(label, outcome string) string {
	if strings.HasPrefix(outcome, "failed") {
		return s.heading(label) + ": " + s.outcome(outcome)
	}
	mark, attribute := outcomeStyle(outcome)
	return s.paint(mark, attribute) + " " + s.paint(label, color.Bold) + ": " + s.outcome(outcome)
}

func printSuccess(out io.Writer, message string) error {
	style := newTextStyle(out)
	_, err := fmt.Fprintln(out, style.paint("✓", color.FgHiGreen)+" "+changes.Text(message))
	return err
}
