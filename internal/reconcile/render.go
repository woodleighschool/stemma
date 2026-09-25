package reconcile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

const (
	planContext  = "stemma/plan"
	applyContext = "stemma/apply"
	// failedDescription stands in for a failure whose text a status must not
	// carry.
	failedDescription = "apply failed; see the reconcile run"
)

// Pull requests and commit statuses outlive the run, so they name resources,
// destinations and fields but never repeat values or error text: scripts and
// plugin errors can carry environment values. The run's report has the detail.

// applySummary condenses an apply into a status line. The summary is empty
// when the apply stopped without a resource to name.
func applySummary(report engine.Report, err error) (state, summary string) {
	var failed []string
	changes, blocked := 0, 0
	for _, resource := range report.Resources {
		if len(resource.BlockedBy) > 0 {
			blocked++
		} else if resource.Error != "" {
			failed = append(failed, resource.Name)
		}
		for _, destination := range resource.Destinations {
			if destination.Applied {
				changes += len(destination.Changes)
			}
		}
	}
	switch {
	case err == nil:
		return sourcecontrol.Success, fmt.Sprintf("%s, %s", plural(len(report.Resources), "resource"), plural(changes, "destination change"))
	case len(failed) > 0:
		summary := fmt.Sprintf("%s failed: %s", plural(len(failed), "resource"), strings.Join(failed, ", "))
		if blocked > 0 {
			summary += "; " + plural(blocked, "resource") + " blocked"
		}
		return sourcecontrol.Failure, summary
	default:
		return sourcecontrol.Failure, ""
	}
}

func plural(count int, noun string) string {
	if count == 1 {
		return strconv.Itoa(count) + " " + noun
	}
	return strconv.Itoa(count) + " " + noun + "s"
}

// verification is the outcome of preparing and planning a proposal.
type verification struct {
	state, summary, version string
	prepared, planned       engine.Report
	err                     error
}

func (v verification) failed() bool { return v.err != nil }

// publishes reports whether the verified plan changes any destination.
func (v verification) publishes() bool {
	for _, resource := range v.planned.Resources {
		for _, destination := range resource.Destinations {
			if len(destination.Changes) > 0 {
				return true
			}
		}
	}
	return false
}

// title names what merging the proposal does.
func title(change change, v verification) string {
	name := change.kind + "/" + change.name
	switch {
	case change.removed:
		return "Remove " + name
	case change.refresh && !v.failed() && !v.publishes():
		return "Refresh " + name + " lock metadata"
	case !change.refresh && v.version != "":
		return "Update " + name + " to " + v.version
	default:
		return "Update " + name
	}
}

func commitMessage(change change) string {
	name := change.kind + "/" + change.name
	switch {
	case change.removed:
		return "chore(stemma): remove " + name + " lock"
	case change.refresh:
		return "chore(stemma): refresh " + name + " lock metadata"
	default:
		return "chore(stemma): update " + name + " inputs"
	}
}

// body renders the pull request description: what changes in the lock, what
// the proposal prepares and what merging does to each destination.
func body(change change, before, after map[string]source.Entry, v verification) string {
	var text strings.Builder
	name := code(change.kind + "/" + change.name)
	switch {
	case change.removed:
		fmt.Fprintf(&text, "%s is no longer declared in the catalog.\n\n", name)
	case len(before) == 0:
		fmt.Fprintf(&text, "%s is new in the catalog.\n\n", name)
	case change.refresh:
		fmt.Fprintf(&text, "Stemma refreshed the lock for %s.\n\n", name)
	default:
		fmt.Fprintf(&text, "Stemma found an update for %s.\n\n", name)
	}
	text.WriteString("| Area | Change |\n| --- | --- |\n")
	fmt.Fprintf(&text, "| Source | %s |\n", sourceChange(change, before, after))
	prepared := v.planned
	if prepared.Resources == nil {
		prepared = v.prepared
	}
	for _, resource := range prepared.Resources {
		if resource.Kind == change.kind && resource.Name == change.name && len(resource.Artifacts) > 0 {
			fmt.Fprintf(&text, "| Prepared | %s |\n", artifacts(resource))
		}
	}
	// The proposal's own destinations come first, then its dependents'.
	resources := slices.Clone(v.planned.Resources)
	slices.SortStableFunc(resources, func(a, b engine.ResourceReport) int {
		return boolOrder(a.Kind != change.kind || a.Name != change.name, b.Kind != change.kind || b.Name != change.name)
	})
	for _, resource := range resources {
		for _, destination := range resource.Destinations {
			area := cell(destination.Name)
			if resource.Kind != change.kind || resource.Name != change.name {
				area += " (" + code(resource.Kind+"/"+resource.Name) + ")"
			}
			fmt.Fprintf(&text, "| %s | %s |\n", area, destinationChange(destination))
		}
	}
	text.WriteString("\n### Verification\n\n")
	if v.failed() {
		fmt.Fprintf(&text, "Failed: %s. The reconcile report has the details.\n\nStemma verifies this proposal again on its next run.\n", strings.Join(failures(change, v, code), "; "))
	} else {
		text.WriteString("Passed.\n")
		if !change.removed {
			text.WriteString("\nMerging applies the reviewed lock offline from the cache warmed by this run.\n")
		}
	}
	text.WriteString("\n---\n\n")
	text.WriteString("♻ **Rebasing**: Whenever the reviewed branch moves, unless someone else pushes to this branch.\n\n")
	text.WriteString("🔕 **Ignore**: Close this PR and Stemma won't propose these inputs again.\n")
	return text.String()
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

// sourceChange summarises the lock difference by input.
func sourceChange(change change, before, after map[string]source.Entry) string {
	switch {
	case change.removed:
		return "Locked inputs removed"
	case change.refresh:
		return "Lock metadata refreshed; content unchanged"
	}
	var parts []string
	for _, input := range lockfile.DiffInputs(map[string]map[string]source.Entry{"": before}, map[string]map[string]source.Entry{"": after}) {
		name := code(input.Input)
		switch {
		case input.Before == nil:
			parts = append(parts, name+" added: "+code(input.After.Content.Filename))
		case input.After == nil:
			parts = append(parts, name+" removed")
		case !input.ContentChanged:
			parts = append(parts, name+": lock metadata refreshed")
		case input.Before.Content.Filename != input.After.Content.Filename:
			parts = append(parts, name+": "+code(input.Before.Content.Filename)+" → "+code(input.After.Content.Filename))
		default:
			parts = append(parts, name+": new content for "+code(input.After.Content.Filename))
		}
	}
	return strings.Join(parts, "<br>")
}

func artifacts(resource engine.ResourceReport) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(resource.Artifacts)) {
		artifact := resource.Artifacts[name]
		part := code(artifact.Filename)
		if artifact.Version != "" {
			part += " (" + cell(artifact.Version) + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "<br>")
}

func destinationChange(destination engine.DestinationReport) string {
	switch {
	case destination.Error != "":
		return "Planning failed"
	case len(destination.Changes) == 0:
		return "No changes"
	}
	return effects(destination.Changes)
}

// effects summarises destination changes by action: what is created, uploaded,
// updated and deleted, naming fields without their values.
func effects(items []plugin.Change) string {
	var verbs []string
	fields := map[string][]string{}
	for _, change := range items {
		verb, field := change.Action, code(change.Field)
		switch verb {
		case "create", "upload":
		case "delete":
			if before := scalar(change.Before); before != "" {
				field += " (" + cell(before) + ")"
			}
		default:
			verb = "update"
		}
		if !slices.Contains(verbs, verb) {
			verbs = append(verbs, verb)
		}
		if !slices.Contains(fields[verb], field) {
			fields[verb] = append(fields[verb], field)
		}
	}
	phrases := make([]string, 0, len(verbs))
	for _, verb := range verbs {
		if names := fields[verb]; verb == "update" && len(names) > 3 {
			phrases = append(phrases, "update "+plural(len(names), "field"))
		} else {
			phrases = append(phrases, verb+" "+strings.Join(names, ", "))
		}
	}
	text := strings.Join(phrases, "; ")
	return strings.ToUpper(text[:1]) + text[1:]
}

// scalar returns a short single-line value that identifies what a deletion
// removes, such as a version.
func scalar(data json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if len(data) == 0 || decoder.Decode(&value) != nil {
		return ""
	}
	switch value := value.(type) {
	case string:
		if len(value) <= 64 && !strings.Contains(value, "\n") {
			return value
		}
	case json.Number:
		return value.String()
	}
	return ""
}

// failures names what did not verify, formatting identities with quote.
func failures(change change, v verification, quote func(string) string) []string {
	if change.removed {
		return []string{"the catalog does not validate without " + quote(change.kind+"/"+change.name)}
	}
	var parts []string
	for _, resource := range v.prepared.Resources {
		name := quote(resource.Kind + "/" + resource.Name)
		switch {
		case len(resource.BlockedBy) > 0:
			var blockers []string
			for _, key := range resource.BlockedBy {
				kind, name := splitKey(key)
				blockers = append(blockers, quote(kind+"/"+name))
			}
			parts = append(parts, name+" is blocked by "+strings.Join(blockers, ", "))
		case resource.Error != "":
			parts = append(parts, name+" could not be prepared")
		}
	}
	for _, resource := range v.planned.Resources {
		name := quote(resource.Kind + "/" + resource.Name)
		if resource.Error != "" && len(resource.Destinations) == 0 {
			parts = append(parts, name+" could not be prepared offline")
		}
		for _, destination := range resource.Destinations {
			if destination.Error != "" {
				parts = append(parts, quote(destination.Name)+" could not plan "+name)
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "verification stopped before it produced a result")
	}
	return parts
}

// planSummary condenses a verification into a status line.
func planSummary(change change, v verification) string {
	name := change.kind + "/" + change.name
	if v.failed() {
		return "failure: " + strings.Join(failures(change, v, func(text string) string { return text }), "; ")
	}
	if change.removed {
		return "lock maintenance: removed " + name
	}
	changes, destinations := 0, 0
	for _, resource := range v.planned.Resources {
		for _, destination := range resource.Destinations {
			destinations++
			changes += len(destination.Changes)
		}
	}
	label := name
	if v.version != "" {
		label += " " + v.version
	}
	return fmt.Sprintf("%s: %s across %s", label, plural(changes, "planned change"), plural(destinations, "destination"))
}

// code formats text as a Markdown code span inside a table cell.
func code(text string) string {
	return "`" + strings.ReplaceAll(cell(text), "`", "'") + "`"
}

// cell escapes text for a Markdown table cell.
func cell(text string) string {
	return strings.ReplaceAll(changes.Text(text), "|", `\|`)
}
