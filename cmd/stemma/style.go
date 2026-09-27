package main

import (
	"io"
	"os"
	"slices"
	"strings"

	"github.com/fatih/color"
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

// outcome colours a status by what it means for the reader.
func (s textStyle) outcome(text string) string {
	attribute := color.FgHiGreen
	switch {
	case strings.HasPrefix(text, "failed"):
		attribute = color.FgHiRed
	case slices.Contains([]string{"blocked", "skipped", "declined"}, text):
		attribute = color.FgHiYellow
	case slices.Contains([]string{"unchanged", "inputs unchanged", "already applied", "cached", "retired", "no artwork", "no icon declared", "pinned"}, text):
		attribute = color.Faint
	}
	return s.paint(text, attribute)
}
