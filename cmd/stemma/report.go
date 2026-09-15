package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/fatih/color"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/signature"
)

func resourceName(resource engine.ResourceReport) string {
	if resource.Kind == "" {
		return resource.Name
	}
	return resource.Kind + "/" + resource.Name
}

func resourceStatus(method string, resource engine.ResourceReport) string {
	if resource.Error != "" {
		return "failed"
	}
	if method == "update" {
		return "inputs resolved"
	}
	if method == "signature" {
		return "signer derived"
	}
	if len(resource.Destinations) > 0 {
		changes := 0
		for _, destination := range resource.Destinations {
			if destination.Error != "" {
				return "failed"
			}
			changes += len(destination.Changes)
		}
		if changes == 0 {
			return "unchanged"
		}
		if method == "apply" {
			return fmt.Sprintf("applied (%d changes)", changes)
		}
		return fmt.Sprintf("%d planned changes", changes)
	}
	if resource.Cached {
		return "cached"
	}
	return "prepared"
}

func printResource(out io.Writer, method string, resource engine.ResourceReport) error {
	style := newTextStyle(out)
	var details strings.Builder
	for _, destination := range resource.Destinations {
		switch {
		case destination.Error != "":
			_, _ = fmt.Fprintf(&details, "  %s: failed: %s\n", destination.Name, style.paint(destination.Error, color.FgHiRed))
		case len(destination.Changes) > 0:
			verb := "planned"
			if destination.Applied {
				verb = "applied"
			}
			_, _ = fmt.Fprintf(&details, "  %s: %s\n", destination.Name, style.paint(fmt.Sprintf("%d changes %s", len(destination.Changes), verb), color.FgHiGreen))
		case resource.Error != "":
			_, _ = fmt.Fprintf(&details, "  %s: unchanged\n", destination.Name)
		}
		for _, change := range destination.Changes {
			_, _ = fmt.Fprintf(&details, "    %s %s\n", change.Action, change.Field)
		}
	}
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "%s: %s\n", style.paint(resourceName(resource), color.Bold), style.outcome(resourceStatus(method, resource)))
	if details.Len() == 0 && resource.Error != "" {
		_, _ = fmt.Fprintf(&text, "  %s\n", resource.Error)
	}
	text.WriteString(details.String())
	if method == "signature" && resource.Error == "" {
		text.WriteString(signatureDetails(resource))
	}
	_, err := io.WriteString(out, text.String())
	return err
}

// signatureDetails renders the derived signer and the fragment to author.
func signatureDetails(resource engine.ResourceReport) string {
	var text strings.Builder
	for _, name := range slices.Sorted(maps.Keys(resource.Artifacts)) {
		data, ok := resource.Artifacts[name].Evidence["signature"]
		if !ok {
			continue
		}
		var result signature.Result
		if err := json.Unmarshal(data, &result); err != nil {
			continue
		}
		_, _ = fmt.Fprintf(&text, "  Signer:    %s (%s)\n  Target:    %s\n", result.Name, result.Authority, result.Target)
		for line := range strings.SplitSeq(strings.TrimSuffix(result.Fragment(), "\n"), "\n") {
			_, _ = fmt.Fprintf(&text, "  %s\n", line)
		}
	}
	return text.String()
}

func printSummary(out io.Writer, method string, report engine.Report) error {
	style := newTextStyle(out)
	var text strings.Builder
	prepared, cached, failed, changes := 0, 0, 0, 0
	for _, resource := range report.Resources {
		switch {
		case resource.Error != "":
			failed++
		case resource.Cached:
			cached++
		default:
			prepared++
		}
		for _, destination := range resource.Destinations {
			if method == "plan" || destination.Applied {
				changes += len(destination.Changes)
			}
		}
	}
	if report.LockChanged != nil || len(report.Resources) > 0 {
		switch method {
		case "prepare":
			_, _ = fmt.Fprintf(&text, "%s %d prepared, %d cached, %d failed.\n", style.paint("Preparation:", color.Bold), prepared, cached, failed)
		case "signature":
			_, _ = fmt.Fprintf(&text, "%s %d derived, %d failed.\n", style.paint("Signatures:", color.Bold), prepared, failed)
		case "plan":
			_, _ = fmt.Fprintf(&text, "%s %d changes, %d failed resources.\n", style.paint("Plan:", color.Bold), changes, failed)
		case "apply":
			_, _ = fmt.Fprintf(&text, "%s %d changes applied, %d failed resources.\n", style.paint("Apply:", color.Bold), changes, failed)
		}
	}
	if (method == "update" || method == "prepare") && report.LockChanged != nil {
		if *report.LockChanged {
			text.WriteString(style.paint("Lockfile updated.", color.FgHiGreen) + "\n")
		} else {
			text.WriteString(style.paint("Lockfile unchanged.", color.Faint) + "\n")
		}
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func (s textStyle) outcome(text string) string {
	attribute := color.FgHiGreen
	switch text {
	case "failed":
		attribute = color.FgHiRed
	case "interrupted", "not completed":
		attribute = color.FgHiYellow
	}
	return s.paint(text, attribute)
}
