package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/intunewin"
	"github.com/woodleighschool/stemma/internal/lockfile"
	pluginstore "github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/reconcile"
	"github.com/woodleighschool/stemma/internal/signature"
)

func resourceName(resource engine.ResourceReport) string {
	if resource.Kind == "" {
		return resource.Name
	}
	return resource.Kind + "/" + resource.Name
}

// keyName shortens a resource key, which includes the API version, to Kind/name.
func keyName(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return key
}

// selected reports whether a resource belongs in a report without --all:
// failures, and whatever the method changed or derived.
func selected(method string, resource engine.ResourceReport) bool {
	include := resource.Error != "" || len(resource.BlockedBy) > 0 || len(resource.Inputs) > 0
	switch method {
	case "signature":
		include = true
	case "prepare":
		include = include || !resource.Cached
	case "icon":
		include = include || resource.Icon != "" && resource.Icon != "unchanged" && resource.Icon != "no icon declared"
	}
	for _, destination := range resource.Destinations {
		include = include || destination.Error != "" || len(destination.Changes) > 0
	}
	return include
}

func selectReport(report engine.Report, method string, all bool) engine.Report {
	if all {
		return report
	}
	resources := make([]engine.ResourceReport, 0, len(report.Resources))
	for _, resource := range report.Resources {
		if selected(method, resource) {
			resources = append(resources, resource)
		}
	}
	report.Resources = resources
	return report
}

// resourceStatus names a resource's outcome for its block heading.
func resourceStatus(method string, resource engine.ResourceReport) string {
	switch {
	case len(resource.BlockedBy) > 0:
		return "blocked"
	case resource.Error != "":
		return "failed"
	case method == "update" && len(resource.Inputs) > 0:
		return quantity(len(resource.Inputs), "input") + " changed"
	case method == "update":
		return "inputs unchanged"
	case method == "signature":
		return "signer derived"
	case method == "icon" && resource.Icon != "":
		return resource.Icon
	}
	if len(resource.Destinations) > 0 {
		changed := 0
		for _, destination := range resource.Destinations {
			changed += len(destination.Changes)
		}
		switch {
		case changed == 0:
			return "unchanged"
		case method == "apply":
			return quantity(changed, "change") + " applied"
		default:
			return quantity(changed, "planned change")
		}
	}
	if resource.Cached {
		return "cached"
	}
	return "prepared"
}

func destinationStatus(destination engine.DestinationReport) string {
	switch {
	case destination.Error != "" && len(destination.Changes) > 0:
		return "failed (changes not confirmed)"
	case destination.Error != "":
		return "failed"
	case len(destination.Changes) == 0:
		return "unchanged"
	case destination.Applied:
		return quantity(len(destination.Changes), "change") + " applied"
	default:
		return quantity(len(destination.Changes), "planned change")
	}
}

// resourceHeading names a resource and its outcome.
func resourceHeading(style textStyle, method string, resource engine.ResourceReport) string {
	return style.paint(changes.Text(resourceName(resource)), color.Bold) + ": " + style.outcome(resourceStatus(method, resource)) + "\n"
}

// renderResource renders one resource's report block.
func renderResource(style textStyle, method string, resource engine.ResourceReport) string {
	var text strings.Builder
	text.WriteString(resourceHeading(style, method, resource))
	for _, input := range resource.Inputs {
		for index, line := range changes.InputLines(input) {
			if index == 0 {
				line = style.paint(line, inputColour(input))
			}
			fmt.Fprintf(&text, "  %s\n", line)
		}
	}
	switch {
	case len(resource.BlockedBy) > 0:
		names := make([]string, 0, len(resource.BlockedBy))
		for _, key := range resource.BlockedBy {
			names = append(names, changes.Text(keyName(key)))
		}
		fmt.Fprintf(&text, "  blocked by %s\n", strings.Join(names, ", "))
	case resource.Error != "" && len(resource.Destinations) == 0:
		writeError(&text, style, "  ", resource.Error)
	}
	for _, destination := range resource.Destinations {
		fmt.Fprintf(&text, "  %s: %s\n", changes.Text(destination.Name), style.outcome(destinationStatus(destination)))
		for _, change := range destination.Changes {
			for index, line := range changes.Lines(change) {
				fmt.Fprintf(&text, "    %s\n", changeLine(style, change.Action, index, line))
			}
		}
		if destination.Error != "" {
			writeError(&text, style, "    ", destination.Error)
		}
	}
	if resource.Error == "" {
		switch method {
		case "signature":
			text.WriteString(signatureDetails(resource))
		case "prepare":
			for _, name := range slices.Sorted(maps.Keys(resource.Artifacts)) {
				artifact := resource.Artifacts[name]
				fmt.Fprintf(&text, "  %s: %s", changes.Text(name), changes.Text(artifact.Filename))
				if artifact.Version != "" {
					fmt.Fprintf(&text, " (%s)", changes.Text(artifact.Version))
				}
				text.WriteByte('\n')
			}
		}
	}
	text.WriteByte('\n')
	return text.String()
}

// changeLine colours a change's first line by its action and collection
// members by whether they are added or removed.
func changeLine(style textStyle, action string, index int, line string) string {
	switch {
	case index == 0 && action == "create":
		return style.paint(line, color.FgHiGreen)
	case index == 0 && (action == "delete" || action == "clear"):
		return style.paint(line, color.FgHiRed)
	case index == 0:
		return style.paint(line, color.FgHiYellow)
	case strings.HasPrefix(strings.TrimLeft(line, " "), "+ "):
		return style.paint(line, color.FgGreen)
	case strings.HasPrefix(strings.TrimLeft(line, " "), "- "):
		return style.paint(line, color.FgRed)
	}
	return line
}

func writeError(text *strings.Builder, style textStyle, indent, message string) {
	for index, line := range errorLines(message) {
		if index == 0 {
			line = "error: " + line
		}
		fmt.Fprintf(text, "%s%s\n", indent, style.paint(line, color.FgHiRed))
	}
}

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
		fmt.Fprintf(&text, "  Signer: %s (%s)\n  Target: %s\n", changes.Text(result.Name), changes.Text(result.Authority), changes.Text(result.Target))
		for line := range strings.SplitSeq(strings.TrimSuffix(result.Fragment(), "\n"), "\n") {
			fmt.Fprintf(&text, "  %s\n", changes.Text(line))
		}
	}
	return text.String()
}

// printReportEnd ends a human report below the resources that streamed: the
// lock entries of resources no longer declared, then the whole run's totals.
func printReportEnd(out io.Writer, method string, report engine.Report, runErr error) error {
	style := newTextStyle(out)
	var text strings.Builder
	text.WriteString(renderRemovedInputs(style, report.RemovedInputs))
	text.WriteString(renderSummary(style, method, report, runErr))
	_, err := io.WriteString(out, text.String())
	return err
}

func renderRemovedInputs(style textStyle, inputs []lockfile.InputChange) string {
	var text strings.Builder
	last := ""
	for _, input := range inputs {
		if input.Resource != last {
			if last != "" {
				text.WriteByte('\n')
			}
			fmt.Fprintf(&text, "%s: %s\n", style.paint(changes.Text(keyName(input.Resource)), color.Bold), style.outcome("no longer declared"))
			last = input.Resource
		}
		for index, line := range changes.InputLines(input) {
			if index == 0 {
				line = style.paint(line, inputColour(input))
			}
			fmt.Fprintf(&text, "  %s\n", line)
		}
	}
	if last != "" {
		text.WriteByte('\n')
	}
	return text.String()
}

func inputColour(input lockfile.InputChange) color.Attribute {
	switch {
	case input.Before == nil:
		return color.FgHiGreen
	case input.After == nil:
		return color.FgHiRed
	case input.ContentChanged:
		return color.FgHiYellow
	}
	return color.Faint
}

func renderSummary(style textStyle, method string, report engine.Report, runErr error) string {
	s := report.Summary
	var text strings.Builder
	label := map[string]string{"update": "Update", "prepare": "Preparation", "signature": "Signatures", "icon": "Icons", "plan": "Plan", "apply": "Apply"}[method]
	switch {
	case errors.Is(runErr, context.Canceled):
		label += " interrupted"
	case runErr != nil || report.Error != "":
		label += " incomplete"
	}
	text.WriteString(style.paint(label+":", color.Bold) + " ")
	switch method {
	case "update":
		fmt.Fprintf(&text, "%s, %s checked", quantity(s.InputChanges, "input change"), quantity(s.Resources, "resource"))
	case "prepare":
		fmt.Fprintf(&text, "%d prepared, %d cached", s.Prepared, s.Cached)
	case "signature":
		fmt.Fprintf(&text, "%d derived", s.Prepared+s.Cached)
	case "icon":
		fmt.Fprintf(&text, "%d created, %d unchanged", s.Changed, s.Unchanged)
	case "plan":
		fmt.Fprintf(&text, "%s with changes across %s, %d unchanged", quantity(s.Changed, "resource"), quantity(s.Destinations, "destination"), s.Unchanged)
	case "apply":
		fmt.Fprintf(&text, "%s applied, %s unchanged", quantity(s.Applied, "destination"), quantity(s.Unchanged, "resource"))
	}
	if s.Failed > 0 {
		text.WriteString(", " + style.paint(fmt.Sprintf("%d failed", s.Failed), color.FgHiRed))
	}
	if s.Blocked > 0 {
		text.WriteString(", " + style.paint(fmt.Sprintf("%d blocked", s.Blocked), color.FgHiYellow))
	}
	text.WriteString(".\n")
	if (method == "update" || method == "prepare") && report.LockChanged != nil {
		text.WriteString(lockfileStatus(style, *report.LockChanged) + "\n")
	}
	return text.String()
}

func quantity(count int, noun string) string {
	if count != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", count, noun)
}

func lockfileStatus(style textStyle, changed bool) string {
	if changed {
		return style.paint("Lockfile updated.", color.FgHiGreen)
	}
	return style.paint("Lockfile unchanged.", color.Faint)
}

// renderReviewed describes the reviewed commit's publication, whose resources
// streamed as they applied.
func renderReviewed(style textStyle, report reconcile.Report) string {
	apply := report.Apply
	if apply == nil {
		return ""
	}
	head := report.Head
	if len(head) > 12 {
		head = head[:12]
	}
	if report.Branch != "" {
		head = report.Branch + "@" + head
	}
	outcome := "applied"
	switch {
	case apply.Skipped:
		outcome = "already applied"
	case apply.Failed():
		outcome = "failed"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "%s %s %s", style.paint("Reviewed:", color.Bold), changes.Text(head), style.outcome(outcome))
	if apply.Summary != "" {
		fmt.Fprintf(&text, " (%s)", changes.Text(apply.Summary))
	}
	text.WriteByte('\n')
	// Resource failures streamed as they happened; the error is the rest.
	if apply.Error != "" {
		writeError(&text, style, "  ", apply.Error)
	}
	return text.String()
}

// renderProposal describes one proposal branch's outcome.
func renderProposal(style textStyle, proposal reconcile.Proposal) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s: %s", style.paint(changes.Text(keyName(proposal.Resource)), color.Bold), style.outcome(proposal.Action))
	if proposal.PullRequest != "" {
		fmt.Fprintf(&text, " %s", changes.Text(proposal.PullRequest))
	}
	text.WriteByte('\n')
	if proposal.Error != "" {
		writeError(&text, style, "  ", proposal.Error)
	} else if proposal.Summary != "" {
		fmt.Fprintf(&text, "  %s\n", changes.Text(proposal.Summary))
	}
	return text.String()
}

// printReconcileEnd counts the proposals, which streamed as they finished, or
// shows why the update phase stopped. A run that never reached the phase has
// no proposals to count.
func printReconcileEnd(out io.Writer, report reconcile.Report, runErr error) error {
	update := report.Update
	if update == nil {
		return nil
	}
	style := newTextStyle(out)
	label := "Proposals:"
	if errors.Is(runErr, context.Canceled) {
		label = "Proposals interrupted:"
	}
	var text strings.Builder
	text.WriteString(style.paint(label, color.Bold) + " ")
	if update.Error != "" {
		text.WriteString(style.outcome("failed") + "\n")
		writeError(&text, style, "  ", update.Error)
	} else {
		var counts []string
		actions := map[string]int{}
		for _, proposal := range update.Proposals {
			if actions[proposal.Action] == 0 {
				counts = append(counts, proposal.Action)
			}
			actions[proposal.Action]++
		}
		for i, action := range counts {
			counts[i] = fmt.Sprintf("%d %s", actions[action], action)
		}
		if len(counts) == 0 {
			counts = []string{"none"}
		}
		text.WriteString(strings.Join(counts, ", ") + ".\n")
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func printPlugins(out io.Writer, plugins map[string]config.Plugin) error {
	style := newTextStyle(out)
	var text strings.Builder
	for _, name := range slices.Sorted(maps.Keys(plugins)) {
		declaration := plugins[name]
		source := declaration.Image + declaration.Path
		if declaration.Entrypoint != "" {
			source += " (" + declaration.Entrypoint + ")"
		}
		fmt.Fprintf(&text, "%s: %s\n", style.paint(changes.Text(name), color.Bold), changes.Text(source))
	}
	if len(plugins) == 0 {
		text.WriteString("No plugins configured.\n")
	}
	_, err := io.WriteString(out, text.String())
	return err
}

// publishedPlugin reports a publication: the pin stemma plugins install
// records for the tag, and the runner platforms it serves.
type publishedPlugin struct {
	pluginstore.Entry

	Platforms []string `json:"platforms"`
}

// printLockedPlugins names the code each plugin is locked to and whether this
// run changed it.
func printLockedPlugins(out io.Writer, previous, locked map[string]pluginstore.Entry, changed bool) error {
	style := newTextStyle(out)
	digest := func(entry pluginstore.Entry) string {
		if entry.Local != nil {
			return "sha256:" + entry.Local.Content.Artifact.SHA256
		}
		return entry.Digest
	}
	var text strings.Builder
	for _, name := range slices.Sorted(maps.Keys(locked)) {
		entry := locked[name]
		outcome := "unchanged"
		if before, ok := previous[name]; !ok {
			outcome = "locked"
		} else if digest(before) != digest(entry) {
			outcome = "updated"
		}
		id := digest(entry)
		if len(id) > 19 {
			id = id[:19]
		}
		fmt.Fprintf(&text, "%s: %s %s %s\n", style.paint(changes.Text(name), color.Bold), changes.Text(entry.Image+entry.Path), id, style.outcome(outcome))
	}
	text.WriteString(lockfileStatus(style, changed) + "\n")
	_, err := io.WriteString(out, text.String())
	return err
}

// renderInspection describes an inspected artifact, then the facts of each
// subject in it. --json carries the complete model.
func renderInspection(style textStyle, inspection engine.Inspection) string {
	var text strings.Builder
	var digest string
	if subjects := inspection.Facts.Subjects; len(subjects) > 0 && subjects[0].ID == "." {
		digest = subjects[0].SHA256
	}
	writeFields(&text, style, inspection.Filename, [][2]string{{"Format", inspection.Format}, {"Version", inspection.Version}, {"SHA-256", digest}})
	for _, subject := range inspection.Facts.Subjects {
		name := ""
		if subject.Path != "." {
			name = " " + subject.Path
		}
		if app := subject.App; app != nil {
			writeFields(&text, style, "Application"+name, [][2]string{{"Installed path", subject.InstalledPath}, {"Bundle ID", app.BundleID}, {"Name", app.Name}, {"Version", app.Version}, {"Build", app.Build}, {"Executable", app.Executable}, {"Minimum OS", app.MinimumOS}})
		}
		if installer := subject.Installer; installer != nil {
			writeFields(&text, style, "Installer"+name, [][2]string{{"Version", installer.Version}, {"Minimum OS", installer.MinimumOS}, {"Restart action", installer.RestartAction}})
		}
		if receipt := subject.Package; receipt != nil {
			payload := "no"
			if receipt.HasPayload {
				payload = "yes"
			}
			var size string
			if receipt.InstalledSize > 0 {
				size = humanize.IBytes(uint64(receipt.InstalledSize) << 10)
			}
			writeFields(&text, style, "Package"+name, [][2]string{{"Identifier", receipt.Identifier}, {"Version", receipt.Version}, {"Install location", receipt.InstallLocation}, {"Installed size", size}, {"Payload", payload}})
		}
		if msi := subject.MSI; msi != nil {
			writeFields(&text, style, "MSI"+name, [][2]string{{"Product name", msi.ProductName}, {"Product version", msi.ProductVersion}, {"Manufacturer", msi.Manufacturer}, {"Product code", msi.ProductCode}, {"Upgrade code", msi.UpgradeCode}, {"Package code", msi.PackageCode}})
		}
		if name != "" && subject.App == nil && subject.Installer == nil && subject.Package == nil && subject.MSI == nil {
			// A package in a disk image or tree is listed without being read.
			writeFields(&text, style, strings.ToUpper(subject.Kind[:1])+subject.Kind[1:]+name, nil)
		}
	}
	return text.String()
}

// renderEnvelope describes an Intune Win32 envelope.
func renderEnvelope(style textStyle, filename string, envelope intunewin.Metadata) string {
	var text strings.Builder
	writeFields(&text, style, filename, [][2]string{
		{"Format", "intunewin"},
		{"Name", envelope.Name},
		{"Setup file", envelope.SetupFile},
		{"Payload size", humanize.IBytes(uint64(max(0, envelope.PlaintextSize)))},
		{"Payload SHA-256", envelope.PayloadSHA256},
		{"Encrypted size", humanize.IBytes(uint64(max(0, envelope.EncryptedContentSize)))},
	})
	return text.String()
}

// writeFields writes a titled block of aligned fields, leaving out empty ones.
// Blocks after the first start with a blank line.
func writeFields(text *strings.Builder, style textStyle, title string, fields [][2]string) {
	if text.Len() > 0 {
		text.WriteByte('\n')
	}
	text.WriteString(style.paint(changes.Text(title), color.Bold) + "\n")
	width := 0
	for _, field := range fields {
		if field[1] != "" {
			width = max(width, len(field[0])+1)
		}
	}
	for _, field := range fields {
		if field[1] != "" {
			fmt.Fprintf(text, "  %-*s  %s\n", width, field[0]+":", changes.Text(field[1]))
		}
	}
}
