package main

import (
	"io"
	"os"

	"github.com/fatih/color"
	"golang.org/x/term"
)

type textStyle struct {
	enabled bool
}

func terminalOutput(out io.Writer) bool {
	if report, ok := out.(reportWriter); ok {
		out = report.Writer
	}
	file, ok := out.(*os.File)
	return ok && term.IsTerminal(int(file.Fd())) && os.Getenv("TERM") != "dumb"
}

func newTextStyle(out io.Writer) textStyle {
	return textStyle{enabled: terminalOutput(out) && os.Getenv("NO_COLOR") == "" && os.Getenv("CI") == ""}
}

func (s textStyle) paint(text string, attributes ...color.Attribute) string {
	if !s.enabled {
		return text
	}
	style := color.New(attributes...)
	style.EnableColor()
	return style.Sprint(text)
}
