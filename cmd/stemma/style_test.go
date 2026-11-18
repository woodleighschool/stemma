package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/plugin"
)

func TestWrappedReportsKeepOwnersAndCompleteValues(t *testing.T) {
	before := "https://downloads.example.test/releases/Example-123.456-arm64.pkg"
	after := "https://downloads.example.test/releases/Example-123.457-arm64.pkg"
	message := `package "Example-123.457-arm64.pkg" already exists; set package_id to 1563 to adopt it`
	pathError := "mkdir /private/var/folders/example/temporary/catalog/repository/packages/not-a-directory: not a directory"
	scriptBefore := "#!/bin/sh\nprintf 'Original description for 日本語 applications'\n"
	scriptAfter := "#!/bin/sh\nprintf 'Revised description for 日本語 applications'\n"
	raw := func(value string) json.RawMessage {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	resource := engine.ResourceReport{Kind: "MacSoftware", Name: "example", Status: engine.StatusFailed, Publications: []engine.Publication{
		{Destination: "intune", Status: engine.StatusChanged, Version: "2", Previous: "1", Fields: []plugin.FieldChange{
			{Field: "url", Before: raw(before), After: raw(after)},
			{Field: "script", Before: raw(scriptBefore), After: raw(scriptAfter)},
		}},
		{Destination: "jamf", Status: engine.StatusFailed, Error: message + "\n" + pathError},
	}}
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	compact := func(value string) string { return strings.Join(strings.Fields(value), "") }
	for _, width := range []int{48, 100} {
		for _, colour := range []bool{false, true} {
			t.Run(fmt.Sprintf("width=%d/colour=%t", width, colour), func(t *testing.T) {
				text := renderRows(textStyle{enabled: colour, width: width}, resourceName(resource), resource.Status, resourceRows("apply", resource, false))
				plain := ansi.ReplaceAllString(text, "")
				for _, want := range []string{"MacSoftware/example", "intune", "jamf", "url", "script"} {
					if !strings.Contains(plain, want) {
						t.Fatalf("lost subject or field %q:\n%s", want, plain)
					}
				}
				for _, value := range []string{before, after, message, pathError, "Original description for 日本語 applications", "Revised description for 日本語 applications"} {
					if !strings.Contains(compact(plain), compact(value)) {
						t.Fatalf("lost value %q:\n%s", value, plain)
					}
				}
				for line := range strings.SplitSeq(plain, "\n") {
					if runewidth.StringWidth(line) >= width {
						t.Fatalf("line exceeds %d columns: %q", width, line)
					}
				}
			})
		}
	}
}
