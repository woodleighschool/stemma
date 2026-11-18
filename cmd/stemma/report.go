package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/intunewin"
	"github.com/woodleighschool/stemma/internal/reconcile"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
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

// renderRows gives unnamed outcomes to the enclosing subject; named outcomes
// introduce a child scope. Every scope owns its indented details.
func renderRows(style textStyle, subject string, status engine.Status, rows []changes.Row) string {
	var text strings.Builder
	heading := resultHeading(changes.Text(subject), status)
	if len(rows) == 1 && rows[0].Subject == "" {
		style.writeHeading(&text, "", heading, rows[0].Summary)
		for _, detail := range rows[0].Details {
			style.writeLine(&text, "  ", detail)
		}
		return text.String()
	}
	style.writeHeading(&text, "", heading, nil)
	for _, row := range rows {
		indent := "  "
		if row.Subject != "" {
			style.writeHeading(&text, indent, changes.Line{{Role: changes.Plain, Text: changes.Text(row.Subject)}}, row.Summary)
			indent += "  "
		} else {
			style.writeLine(&text, indent, row.Summary)
		}
		for _, detail := range row.Details {
			style.writeLine(&text, indent, detail)
		}
	}
	return text.String()
}

// resourceRows are a resource's rows for the command that ran.
func resourceRows(method string, resource engine.ResourceReport, namedInputs bool) []changes.Row {
	if resource.Status == engine.StatusUnchanged && (method == "update" || method == "plan" || method == "apply") {
		var versions []string
		for _, publication := range resource.Publications {
			if publication.Version != "" {
				versions = append(versions, changes.Text(publication.Destination+" "+publication.Version))
			}
		}
		summary := "unchanged"
		if len(versions) > 0 {
			summary += " (" + strings.Join(versions, ", ") + ")"
		}
		return []changes.Row{{Summary: changes.Line{{Role: changes.Old, Text: summary}}}}
	}
	if len(resource.BlockedBy) > 0 {
		names := make([]string, 0, len(resource.BlockedBy))
		for _, key := range resource.BlockedBy {
			names = append(names, keyName(key))
		}
		return []changes.Row{changes.BlockedBy(names)}
	}
	switch method {
	case "plan", "apply":
		var rows []changes.Row
		failed := false
		for _, publication := range resource.Publications {
			failed = failed || publication.Error != ""
			if publication.Status == "" {
				continue
			}
			rows = append(rows, changes.PublicationRow(publication))
		}
		if resource.Error != "" && !failed {
			rows = append(rows, changes.Failed("", resourceError(resource)))
		}
		if len(rows) > 0 {
			return rows
		}
	}
	if resource.Error != "" {
		return []changes.Row{changes.Failed("", resourceError(resource))}
	}
	if resource.Status == engine.StatusSkipped {
		return []changes.Row{changes.Skipped(resource.Reason)}
	}
	switch method {
	case "update":
		if rows := changes.InputRows(resource.Inputs, namedInputs); len(rows) > 0 {
			return rows
		}
	case "prepare", "signature":
		return []changes.Row{artifactRow(method, resource)}
	case "icon":
		// A resource prepared only for another's icon has no path of its own.
		if resource.Icon != "" {
			role, text := changes.New, "created "+resource.Icon
			if resource.Status == engine.StatusUnchanged {
				role, text = changes.Old, resource.Icon
			}
			return []changes.Row{{Summary: changes.Line{{Role: role, Text: changes.Text(text)}}}}
		}
	}
	return []changes.Row{{Summary: changes.Line{{Role: changes.Old, Text: "unchanged"}}}}
}

// artifactRow names what preparation produced: the installer's version and
// file, or for signature the signers it derived. Details list the other
// outputs, or each subject and the fragment to declare.
func artifactRow(method string, resource engine.ResourceReport) changes.Row {
	installer := resource.Artifacts["installer"]
	role := changes.New
	if resource.Status != engine.StatusChanged {
		role = changes.Old
	}
	var row changes.Row
	if method == "signature" {
		var observations []signature.Observation
		for _, name := range sortedNames(resource.Artifacts) {
			observations = append(observations, signature.Read(resource.Artifacts[name].Evidence).Observed...)
		}
		row.Summary = changes.Line{{Role: changes.Plain, Text: quantity(len(observations), "signature")}}
		for _, o := range observations {
			subject := o.Subject.Path
			if o.Input != "" {
				subject = o.Input + "/" + subject
			}
			signer := o.Authority
			if o.State == "unsigned" {
				signer = "unsigned"
			} else if o.Name != "" {
				signer = o.Name + " (" + o.Authority + ")"
			}
			row.Details = append(row.Details, changes.Line{{Role: changes.Label, Text: changes.Text(subject) + ": "}, {Role: changes.Plain, Text: changes.Text(signer)}})
		}
		fragment := strings.TrimSuffix(signature.Fragment(observations), "\n")
		if fragment != "" {
			row.Details = append(row.Details, changes.Line{{Role: changes.Label, Text: "configuration:"}})
			for line := range strings.SplitSeq(fragment, "\n") {
				row.Details = append(row.Details, changes.Line{{Role: changes.Plain, Text: "  " + line}})
			}
		}
		return row
	}
	row.Summary = changes.Line{{Role: role, Text: changes.Text(installer.Version)}}
	if installer.Version == "" {
		row.Summary = changes.Line{{Role: role, Text: "prepared"}}
	}
	for _, name := range sortedNames(resource.Artifacts) {
		artifact := resource.Artifacts[name]
		row.Details = append(row.Details, changes.Line{{Role: changes.Label, Text: changes.Text(name) + ": "}, {Role: changes.Plain, Text: changes.Text(artifact.Filename)}})
	}
	return row
}

func sortedNames[T any](values map[string]T) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func resourceError(resource engine.ResourceReport) string {
	message := resource.Error
	for _, prefix := range []string{resource.Key, resourceName(resource)} {
		if prefix != "" {
			message = strings.TrimPrefix(strings.TrimPrefix(message, prefix+": "), prefix+" ")
		}
	}
	return message
}

func writeError(text *strings.Builder, style textStyle, indent, message string) {
	style.writeHeading(text, indent, resultHeading("Error", engine.StatusFailed), nil)
	for _, line := range errorLines(message) {
		style.writeLine(text, indent+"  ", changes.Line{{Role: changes.Failure, Text: line}})
	}
}

func quantity(count int, noun string) string {
	if count != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", count, noun)
}

// reportEnd ends a human report below the rows that streamed: the plugin pins
// and lock documents an update changed outside its resources, then the
// summary.
func reportEnd(style textStyle, method string, report engine.Report, streamed bool) string {
	var blocks []string
	for _, change := range report.Plugins {
		blocks = append(blocks, renderRows(style, "plugin "+change.Name, engine.StatusChanged, []changes.Row{changes.PluginRow(change)}))
	}
	reported := map[string]bool{}
	for _, input := range report.Removed {
		if !reported[input.Resource] {
			reported[input.Resource] = true
			blocks = append(blocks, renderRows(style, keyName(input.Resource), engine.StatusChanged, []changes.Row{changes.Undeclared()}))
		}
	}
	var text strings.Builder
	for _, block := range blocks {
		text.WriteString(block)
	}
	if streamed || len(blocks) > 0 {
		text.WriteString("\n")
	}
	text.WriteString(style.line(changes.Summary(method, report.Summary)) + "\n")
	return text.String()
}

// reviewedRow describes the reviewed commit's publication, whose resources
// streamed as they applied: its outcome and time, with the apply's summary
// below.
func reviewedRow(report reconcile.Report) (string, changes.Row) {
	head := report.Head
	if len(head) > 12 {
		head = head[:12]
	}
	if report.Branch != "" {
		head = report.Branch + "@" + head
	}
	apply := report.Apply
	if apply.Error != "" {
		return head, changes.Failed("", apply.Error)
	}
	row := changes.Row{Summary: changes.Line{{Role: changes.New, Text: "applied"}}}
	switch {
	case apply.Skipped:
		row.Summary = changes.Line{{Role: changes.Old, Text: "already applied"}}
	case apply.Failed():
		row.Summary = changes.Line{{Role: changes.Failure, Text: "failed"}}
	}
	if elapsed := apply.Elapsed.Round(time.Second); elapsed > 0 {
		row.Summary = append(row.Summary, changes.Span{Role: changes.Old, Text: " in " + elapsed.String()})
	}
	if apply.Summary != "" {
		row.Details = []changes.Line{{{Role: changes.Old, Text: changes.Text(apply.Summary)}}}
	}
	return head, row
}

// proposalRow describes one proposal branch's outcome.
func proposalRow(proposal reconcile.Proposal) changes.Row {
	if proposal.Error != "" {
		row := changes.Failed("", proposal.Error)
		if proposal.PullRequest != "" {
			row.Details = append(row.Details, changes.Line{{Role: changes.Label, Text: "pull request: "}, {Role: changes.Plain, Text: changes.Text(proposal.PullRequest)}})
		}
		return row
	}
	role := changes.New
	switch proposal.Action {
	case "unchanged":
		role = changes.Old
	case "blocked":
		role = changes.Blocked
	}
	row := changes.Row{Summary: changes.Line{{Role: role, Text: changes.Text(proposal.Action)}}}
	if proposal.PullRequest != "" {
		row.Details = append(row.Details, changes.Line{{Role: changes.Label, Text: "pull request: "}, {Role: changes.Plain, Text: changes.Text(proposal.PullRequest)}})
	}
	if proposal.Summary != "" {
		row.Details = append(row.Details, changes.Line{{Role: changes.Old, Text: changes.Text(proposal.Summary)}})
	}
	return row
}

// proposalsEnd counts the proposals, which streamed as they finished, or
// shows why the update phase stopped.
func proposalsEnd(style textStyle, update *reconcile.Update) string {
	if update.Error != "" {
		var text strings.Builder
		text.WriteString("\n")
		writeError(&text, style, "", update.Error)
		return text.String()
	}
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
	line := style.paint(quantity(len(update.Proposals), "proposal"), color.Bold)
	if len(counts) > 0 {
		line += style.paint(":", color.Bold) + " " + strings.Join(counts, ", ")
	}
	return "\n" + line + ".\n"
}

func printPlugins(out io.Writer, reports []engine.PluginReport) error {
	style := newTextStyle(out)
	var text strings.Builder
	for _, report := range reports {
		fields := [][2]string{{"Image", shortImage(report.Image)}, {"Path", changes.Text(report.Path)}, {"Status", report.Status}}
		if report.Digest != "" && !strings.Contains(report.Image, "@"+report.Digest) {
			fields = append(fields, [2]string{"Digest", changes.ShortDigest(report.Digest)})
		}
		version := report.Version
		if revision := report.Revision; revision != "" {
			hash, dirty, _ := strings.Cut(revision, "+")
			if len(hash) > 12 {
				hash = hash[:12]
			}
			if dirty != "" {
				hash += "+" + dirty
			}
			version += " (" + hash + ")"
		}
		fields = append(fields, [2]string{"Version", version}, [2]string{"Platforms", strings.Join(report.Platforms, ", ")})
		provides := map[string][]string{}
		for _, operation := range report.Operations {
			provides[operation.Kind] = append(provides[operation.Kind], operationLabel(operation.Name, operation.Resource))
		}
		fields = append(fields, [2]string{"Resolvers", strings.Join(provides["resolve"], ", ")}, [2]string{"Resource kinds", strings.Join(provides["resource"], ", ")}, [2]string{"Destinations", strings.Join(provides["reconcile"], ", ")})
		writeFields(&text, style, "", report.Name, fields)
		for _, problem := range pluginProblems(report) {
			writeError(&text, style, "  ", problem)
		}
	}
	if len(reports) == 0 {
		text.WriteString("No plugins configured.\n")
	}
	_, err := io.WriteString(out, text.String())
	return err
}

// publishedPlugin reports a publication: the tagged platform index and the
// runner platforms it serves.
type publishedPlugin struct {
	Image     string   `json:"image"`
	Digest    string   `json:"digest"`
	Platforms []string `json:"platforms"`
}

// pluginUpdate describes what plugins update did to each plugin's lock entry,
// then the entries of plugins no longer declared, then the counts.
func pluginUpdate(style textStyle, update engine.PluginUpdate) string {
	var text strings.Builder
	counts := map[string]int{}
	for _, report := range update.Plugins {
		row := changes.Row{Summary: changes.Line{{Role: changes.Old, Text: "unchanged"}}}
		outcome := "unchanged"
		switch {
		case report.Error != "" || len(report.Unavailable) > 0:
			outcome = "failed"
			row = changes.Failed("", strings.Join(pluginProblems(report), "\n"))
		case !report.Locked:
			outcome = "pinned"
			row.Summary = changes.Line{{Role: changes.Old, Text: "pinned"}}
		case report.Before == "":
			outcome = "locked"
			row.Summary = changes.Line{{Role: changes.New, Text: "new " + changes.ShortDigest(report.Digest)}}
		case report.Before != report.Digest:
			outcome = "updated"
			row.Summary = changes.Line{{Role: changes.Old, Text: changes.ShortDigest(report.Before) + " →"}, {Role: changes.Plain, Text: " "}, {Role: changes.New, Text: changes.ShortDigest(report.Digest)}}
		}
		counts[outcome]++
		status := engine.StatusChanged
		if outcome == "failed" {
			status = engine.StatusFailed
		}
		text.WriteString(renderRows(style, report.Name, status, []changes.Row{row}))
	}
	for _, name := range update.Removed {
		text.WriteString(renderRows(style, name, engine.StatusChanged, []changes.Row{changes.Undeclared()}))
	}
	var parts []string
	for _, outcome := range []string{"updated", "locked", "pinned", "unchanged", "failed"} {
		if counts[outcome] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[outcome], outcome))
		}
	}
	if text.Len() > 0 {
		text.WriteString("\n")
	}
	line := style.paint(quantity(len(update.Plugins), "plugin"), color.Bold)
	if len(parts) > 0 {
		line += style.paint(":", color.Bold) + " " + strings.Join(parts, ", ")
	}
	text.WriteString(line + ".\n")
	return text.String()
}

// pluginProblems names why a plugin did not load, then the operations it
// offers that this Stemma cannot use, grouped by reason.
func pluginProblems(report engine.PluginReport) []string {
	var problems []string
	if report.Error != "" {
		problems = append(problems, report.Error)
	}
	var reasons []string
	unavailable := map[string][]string{}
	for _, operation := range report.Unavailable {
		if _, seen := unavailable[operation.Reason]; !seen {
			reasons = append(reasons, operation.Reason)
		}
		unavailable[operation.Reason] = append(unavailable[operation.Reason], operationLabel(operation.Name, operation.Resource))
	}
	for _, reason := range reasons {
		problems = append(problems, strings.Join(unavailable[reason], ", ")+": "+reason)
	}
	return problems
}

// operationLabel names an operation the way catalogs refer to it: a resource
// kind by its apiVersion and kind, anything else by its name.
func operationLabel(name string, resource *plugin.ResourceKind) string {
	if resource != nil {
		return changes.Text(resource.APIVersion + "/" + resource.Kind)
	}
	return changes.Text(name)
}

// shortDigest keeps enough of a digest to recognise it; --json has all of it.
func shortImage(image string) string {
	if name, digest, pinned := strings.Cut(image, "@"); pinned {
		return changes.Text(name) + "@" + changes.ShortDigest(digest)
	}
	return changes.Text(image)
}

// renderInspection describes an inspected artifact, then the facts of each
// subject in it. --json carries the complete model.
func renderInspection(style textStyle, inspection engine.Inspection) string {
	var text strings.Builder
	fields := [][2]string{{"Format", inspection.Format}, {"Version", inspection.Version}}
	children := map[string][]plugin.Subject{}
	for _, subject := range inspection.Facts.Subjects {
		if subject.ID == "." {
			fields = append(fields, [2]string{"SHA-256", subject.SHA256})
			for _, field := range subjectFields(subject) {
				if field[0] != "Version" || field[1] != inspection.Version {
					fields = append(fields, field)
				}
			}
			continue
		}
		parent := subject.Parent
		if parent == "" {
			parent = "."
		}
		children[parent] = append(children[parent], subject)
	}
	writeFields(&text, style, "", inspection.Filename, fields)
	var writeSubjects func(string, string)
	writeSubjects = func(parent, indent string) {
		for _, subject := range children[parent] {
			name := strings.TrimPrefix(subject.Path, parent+"/")
			writeFields(&text, style, indent, name, subjectFields(subject))
			writeSubjects(subject.ID, indent+"  ")
		}
	}
	writeSubjects(".", "  ")
	return text.String()
}

func subjectFields(subject plugin.Subject) [][2]string {
	var fields [][2]string
	if app := subject.App; app != nil {
		fields = append(fields, [][2]string{{"Installed path", subject.InstalledPath}, {"Bundle ID", app.BundleID}, {"Name", app.Name}, {"Version", app.Version}, {"Build", app.Build}, {"Executable", app.Executable}, {"Architectures", strings.Join(app.Architectures, ", ")}, {"Minimum OS", app.MinimumOS}}...)
	}
	if installer := subject.Installer; installer != nil {
		fields = append(fields, [][2]string{{"Version", installer.Version}, {"Minimum OS", installer.MinimumOS}, {"Restart action", installer.RestartAction}}...)
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
		fields = append(fields, [][2]string{{"Identifier", receipt.Identifier}, {"Version", receipt.Version}, {"Install location", receipt.InstallLocation}, {"Installed size", size}, {"Payload", payload}}...)
	}
	if msi := subject.MSI; msi != nil {
		fields = append(fields, [][2]string{{"Product name", msi.ProductName}, {"Product version", msi.ProductVersion}, {"Manufacturer", msi.Manufacturer}, {"Product code", msi.ProductCode}, {"Upgrade code", msi.UpgradeCode}, {"Package code", msi.PackageCode}}...)
	}
	return fields
}

// renderEnvelope describes an Intune Win32 envelope.
func renderEnvelope(style textStyle, filename string, envelope intunewin.Metadata) string {
	var text strings.Builder
	writeFields(&text, style, "", filename, [][2]string{
		{"Format", "intunewin"},
		{"Name", envelope.Name},
		{"Setup file", envelope.SetupFile},
		{"Payload size", humanize.IBytes(uint64(max(0, envelope.PlaintextSize)))},
		{"Payload SHA-256", envelope.PayloadSHA256},
		{"Encrypted size", humanize.IBytes(uint64(max(0, envelope.EncryptedContentSize)))},
	})
	return text.String()
}

// writeFields emits a scope and its populated fields. An empty title adds
// fields to the existing scope.
func writeFields(text *strings.Builder, style textStyle, indent, title string, fields [][2]string) {
	if title != "" {
		style.writeHeading(text, indent, changes.Line{{Role: changes.Strong, Text: changes.Text(title)}}, nil)
		indent += "  "
	}
	for _, field := range fields {
		if field[1] != "" {
			style.writeLine(text, indent, changes.Line{{Role: changes.Label, Text: field[0] + ": "}, {Role: changes.Plain, Text: changes.Text(field[1])}})
		}
	}
}
