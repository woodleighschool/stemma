package reconcile

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
	"github.com/woodleighschool/stemma/internal/source"
)

const (
	planContext  = "stemma/plan"
	applyContext = "stemma/apply"
	// failedDescription stands in for a failure whose text a status must not
	// carry.
	failedDescription = "apply failed; see the reconcile run"
	// refreshName names the lock refresh in reports.
	refreshName = "Lock refresh"
	noChanges   = "No changes"
)

// Pull requests and commit statuses outlive the run, so they name resources,
// destinations and fields but never repeat values or error text: scripts and
// plugin errors can carry environment values. The run's report has the detail.

// applySummary condenses an apply into a status line. The summary is empty
// when the apply stopped without a resource to name.
func applySummary(report engine.Report, err error) (state, summary string) {
	var failed []string
	blocked := 0
	for _, resource := range report.Resources {
		if len(resource.BlockedBy) > 0 {
			blocked++
		} else if resource.Error != "" {
			failed = append(failed, resource.Name)
		}
	}
	switch {
	case err == nil:
		return sourcecontrol.Success, strings.TrimSuffix(changes.Summary("apply", report.Summary).String(), ".")
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

// planned is the outcome of planning a changeset. The report is absent when
// the set left nothing to plan.
type planned struct {
	report *engine.Report
	err    error
}

func (p planned) failed() bool { return p.err != nil }

func (p planned) state() string {
	if p.failed() {
		return sourcecontrol.Failure
	}
	return sourcecontrol.Success
}

func (p planned) resources() []engine.ResourceReport {
	if p.report == nil {
		return nil
	}
	return p.report.Resources
}

// preparedVersion is the installer version an update prepares, once its plan
// got that far.
func preparedVersion(set changeset, plan planned) string {
	for _, resource := range plan.resources() {
		if resource.Key == set.key {
			return resource.Artifacts["installer"].Version
		}
	}
	return ""
}

// title names what merging the proposal does.
func title(set changeset, plan planned) string {
	if set.refresh() {
		return "Refresh locks"
	}
	if version := preparedVersion(set, plan); version != "" {
		return "Update " + set.name() + " to " + version
	}
	return "Update " + set.name()
}

func commitMessage(set changeset) string {
	if set.refresh() {
		return "chore(stemma): refresh locks"
	}
	return "chore(stemma): update " + set.name() + " inputs"
}

// body renders the pull request description: what changes in the lock and what
// merging does to each destination. A failure appears where it happened; the
// plan's commit status carries the verdict.
func body(set changeset, plan planned) string {
	var text strings.Builder
	ignore := "these inputs again"
	if set.refresh() {
		writeRefresh(&text, set, plan)
		ignore = "it again until one of these entries changes"
	} else {
		writeUpdate(&text, set, plan)
	}
	if plan.failed() {
		text.WriteByte('\n')
		if len(failures(plan)) == 0 {
			text.WriteString("Planning stopped before any resource finished. ")
		}
		text.WriteString("The reconcile report has the details.\n")
	}
	text.WriteString("\n---\n\n")
	text.WriteString("♻ **Rebasing**: Whenever the reviewed branch moves, unless someone else pushes to this branch.\n\n")
	fmt.Fprintf(&text, "🔕 **Ignore**: Close this PR and Stemma won't propose %s.\n", ignore)
	return text.String()
}

// writeUpdate describes one resource's new content: its inputs, what it
// prepares and each destination of the resource and of its consumers.
func writeUpdate(text *strings.Builder, set changeset, plan planned) {
	c := set.changes[set.key]
	if len(c.before) == 0 {
		fmt.Fprintf(text, "%s is new in the catalog.\n\n", code(set.name()))
	} else {
		fmt.Fprintf(text, "Stemma found an update for %s.\n\n", code(set.name()))
	}
	text.WriteString("| Area | Change |\n| --- | --- |\n")
	fmt.Fprintf(text, "| Source | %s |\n", sourceChange(c))
	// The proposal's own resource comes first, then its consumers.
	resources := slices.Clone(plan.resources())
	slices.SortStableFunc(resources, func(a, b engine.ResourceReport) int {
		return boolOrder(a.Key != set.key, b.Key != set.key)
	})
	for _, resource := range resources {
		own := resource.Key == set.key
		name := code(resource.Kind + "/" + resource.Name)
		if own && len(resource.Artifacts) > 0 {
			fmt.Fprintf(text, "| Prepared | %s |\n", artifacts(resource))
		}
		if reason := stopped(resource); reason != "" {
			fmt.Fprintf(text, "| %s | %s |\n", name, reason)
			continue
		}
		for _, publication := range resource.Publications {
			area := cell(publication.Destination)
			if !own {
				area += " (" + name + ")"
			}
			fmt.Fprintf(text, "| %s | %s |\n", area, destinationChange(publication))
		}
	}
}

// writeRefresh describes the lock refresh. A resource whose merge does more
// than rewrite lock metadata gets a row; the others are listed by name.
func writeRefresh(text *strings.Builder, set changeset, plan planned) {
	reports := map[string]engine.ResourceReport{}
	for _, resource := range plan.resources() {
		reports[resource.Key] = resource
	}
	var rows, quiet []string
	stale, unplanned := false, false
	for _, key := range slices.Sorted(maps.Keys(set.changes)) {
		c := set.changes[key]
		name := code(c.kind + "/" + c.name)
		locked, rejected := lockChange(c)
		stale = stale || rejected
		merge := ""
		report, ok := reports[key]
		if ok {
			merge = onMerge(report)
			delete(reports, key)
		}
		if c.removed || rejected || ok && merge != noChanges {
			rows = append(rows, fmt.Sprintf("| %s | %s | %s |\n", name, locked, merge))
			continue
		}
		quiet = append(quiet, name)
		unplanned = unplanned || !ok
	}
	// Consumers of a refreshed resource are planned with it.
	for _, key := range slices.Sorted(maps.Keys(reports)) {
		if merge := onMerge(reports[key]); merge != noChanges {
			rows = append(rows, fmt.Sprintf("| %s | Unchanged | %s |\n", code(reports[key].Kind+"/"+reports[key].Name), merge))
		}
	}

	kept, removed := set.counts()
	switch {
	case kept == 0:
		fmt.Fprintf(text, "Stemma removed the lock entries of %s the catalog no longer declares", plural(removed, "resource"))
	case removed == 0:
		fmt.Fprintf(text, "Stemma refreshed the lock for %s. Every input resolves to the content already locked", plural(kept, "resource"))
	default:
		fmt.Fprintf(text, "Stemma refreshed the lock for %s and removed %d the catalog no longer declares. Every input resolves to the content already locked", plural(kept, "resource"), removed)
	}
	if len(rows) == 0 && !plan.failed() {
		text.WriteString(", and no destination changes")
	}
	text.WriteString(".\n")

	others := plural(len(quiet), "resource")
	if len(rows) > 0 {
		others = plural(len(quiet), "other")
		text.WriteString("\n| Resource | Lock | On merge |\n| --- | --- | --- |\n")
		text.WriteString(strings.Join(rows, ""))
		if len(quiet) > 0 {
			merge := noChanges
			if unplanned {
				merge = ""
			}
			fmt.Fprintf(text, "| %s | Metadata refreshed | %s |\n", others, merge)
		}
	}
	switch {
	case removed > 0:
		text.WriteString("\nThe reviewed branch can't apply until this merges.\n")
	case stale:
		text.WriteString("\nThe reviewed branch can't apply the resources with a changed declaration or resolver until this merges.\n")
	}
	if len(quiet) > 0 {
		fmt.Fprintf(text, "\n<details>\n<summary>%s</summary>\n\n%s\n\n</details>\n", others, strings.Join(quiet, ", "))
	}
}

// lockChange names what a refresh does to a resource's entries. Stale entries
// were locked for another declaration or resolver, so a frozen run rejects
// them.
func lockChange(c change) (text string, stale bool) {
	if c.removed {
		return "Removed", false
	}
	redeclared, reresolved := false, false
	for name, after := range c.after {
		before := c.before[name]
		redeclared = redeclared || before.Declaration != after.Declaration
		reresolved = reresolved || before.Resolver != after.Resolver || before.ResolverVersion != after.ResolverVersion
	}
	switch {
	case redeclared:
		return "Declaration changed", true
	case reresolved:
		return "Resolver changed", true
	}
	return "Metadata refreshed", false
}

// onMerge summarises what merging does to one planned resource.
func onMerge(resource engine.ResourceReport) string {
	if reason := stopped(resource); reason != "" {
		return reason
	}
	var parts []string
	quiet := true
	for _, publication := range resource.Publications {
		quiet = quiet && publication.Status == engine.StatusUnchanged
		parts = append(parts, code(publication.Destination)+": "+destinationChange(publication))
	}
	if quiet {
		return noChanges
	}
	return strings.Join(parts, "<br>")
}

// stopped says why a planned resource never reached its destinations, or
// nothing when it did.
func stopped(resource engine.ResourceReport) string {
	switch {
	case len(resource.BlockedBy) > 0:
		names := blockers(resource)
		for i, name := range names {
			names[i] = code(name)
		}
		return "Blocked by " + strings.Join(names, ", ")
	case resource.Error != "" && len(resource.Publications) == 0:
		return "Preparation failed"
	}
	return ""
}

// blockers names the resources whose outputs a blocked resource waits on.
func blockers(resource engine.ResourceReport) []string {
	names := make([]string, len(resource.BlockedBy))
	for i, key := range resource.BlockedBy {
		kind, name := splitKey(key)
		names[i] = kind + "/" + name
	}
	return names
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

// sourceChange is an update's lock difference, one input per line, in the
// rows every report shows.
func sourceChange(c change) string {
	inputs := lock.DiffInputs(map[string]map[string]source.Entry{"": c.before}, map[string]map[string]source.Entry{"": c.after})
	var parts []string
	for _, row := range changes.InputRows(inputs, len(c.after) > 1 || len(c.before) > 1) {
		part := cell(row.Summary.String())
		if row.Subject != "" {
			part = code(row.Subject) + ": " + part
		}
		parts = append(parts, part)
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

// destinationChange is a publication's headline, with the names of the
// fields it changes but none of their values: pull requests outlive the run,
// and metadata values can carry environment values.
func destinationChange(publication engine.Publication) string {
	if publication.Status == engine.StatusFailed {
		return "planning failed"
	}
	if publication.Status != engine.StatusChanged {
		return "no changes"
	}
	parts := []string{cell(changes.PublicationHeadline(publication).String())}
	var fields []string
	for _, field := range publication.Fields {
		fields = append(fields, code(field.Field))
	}
	if len(fields) > 0 {
		parts = append(parts, "update "+strings.Join(fields, ", "))
	}
	for _, upload := range publication.Uploads {
		parts = append(parts, "upload "+code(upload.Name)+": "+code(upload.Filename))
	}
	for _, repair := range publication.Repairs {
		parts = append(parts, "rebuild "+cell(repair))
	}
	if len(publication.Removed) > 0 {
		parts = append(parts, "remove "+cell(strings.Join(publication.Removed, ", ")))
	}
	return strings.Join(parts, "; ")
}

// failures names what did not plan, without error text.
func failures(plan planned) []string {
	var parts []string
	for _, resource := range plan.resources() {
		name := resource.Kind + "/" + resource.Name
		switch {
		case len(resource.BlockedBy) > 0:
			parts = append(parts, name+" is blocked by "+strings.Join(blockers(resource), ", "))
		case resource.Error != "" && len(resource.Publications) == 0:
			parts = append(parts, name+" could not be prepared")
		}
		for _, publication := range resource.Publications {
			if publication.Error != "" {
				parts = append(parts, publication.Destination+" could not plan "+name)
			}
		}
	}
	return parts
}

// planSummary condenses a plan into a status line.
func planSummary(set changeset, plan planned) string {
	if plan.failed() {
		failed := failures(plan)
		if len(failed) == 0 {
			failed = []string{"planning stopped before any resource finished"}
		}
		return "failure: " + strings.Join(failed, "; ")
	}
	label := set.name()
	if set.refresh() {
		kept, removed := set.counts()
		switch {
		case kept == 0:
			label = plural(removed, "lock") + " removed"
		case removed == 0:
			label = plural(kept, "lock") + " refreshed"
		default:
			label = fmt.Sprintf("%s refreshed, %d removed", plural(kept, "lock"), removed)
		}
	} else if version := preparedVersion(set, plan); version != "" {
		label += " " + version
	}
	if plan.report == nil {
		return label
	}
	planned := 0
	destinations := map[string]bool{}
	for _, resource := range plan.resources() {
		for _, publication := range resource.Publications {
			destinations[publication.Destination] = true
			if publication.Status == engine.StatusChanged {
				planned++
			}
		}
	}
	return fmt.Sprintf("%s: %s across %s", label, plural(planned, "publication"), plural(len(destinations), "destination"))
}

// code formats text as a Markdown code span inside a table cell.
func code(text string) string {
	return "`" + strings.ReplaceAll(cell(text), "`", "'") + "`"
}

// cell escapes text for a Markdown table cell.
func cell(text string) string {
	return strings.ReplaceAll(changes.Text(text), "|", `\|`)
}
