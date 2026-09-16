package reconcile

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
	"github.com/woodleighschool/stemma/internal/source"
)

const (
	planContext  = "stemma/plan"
	applyContext = "stemma/apply"
)

// applySummary condenses an apply into a status line; the report and logs keep the detail.
func applySummary(report engine.Report, err error) (state, summary string) {
	var failed []string
	changes := 0
	for _, resource := range report.Resources {
		if resource.Error != "" {
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
		return sourcecontrol.Failure, fmt.Sprintf("%s failed: %s", plural(len(failed), "resource"), strings.Join(failed, ", "))
	default:
		return sourcecontrol.Failure, firstLine(err.Error())
	}
}

func plural(count int, noun string) string {
	if count == 1 {
		return strconv.Itoa(count) + " " + noun
	}
	return strconv.Itoa(count) + " " + noun + "s"
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

// verification is the outcome of preparing and planning a proposal.
type verification struct {
	state, summary, version string
	prepared, planned       engine.Report
	err                     error
}

func (v verification) failed() bool { return v.err != nil }

func title(change change, v verification) string {
	name := change.kind + "/" + change.name
	if change.removed {
		return "Remove " + name + " from the lockfile"
	}
	if change.refresh {
		return "Refresh " + name + " lock metadata"
	}
	if v.version != "" {
		return "Update " + name + " to " + v.version
	}
	return "Update " + name
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

// body renders the pull request description: what changed in the lock, what
// the change prepares and what publishing it would do.
func body(change change, before, after map[string]source.Entry, v verification) string {
	var text strings.Builder
	name := "`" + change.kind + "/" + change.name + "`"
	switch {
	case change.removed:
		fmt.Fprintf(&text, "%s is no longer declared in the catalog. This removes its locked inputs.\n\n", name)
	case change.refresh:
		fmt.Fprintf(&text, "Stemma resolved the bytes already locked for %s with new metadata.\n\n", name)
	default:
		fmt.Fprintf(&text, "Stemma resolved new inputs for %s.\n\n", name)
	}
	inputs := slices.Sorted(maps.Keys(before))
	for input := range after {
		if _, ok := before[input]; !ok {
			inputs = append(inputs, input)
		}
	}
	sort.Strings(inputs)
	text.WriteString("| Input | Before | After |\n| --- | --- | --- |\n")
	for _, input := range inputs {
		fmt.Fprintf(&text, "| %s | %s | %s |\n", input, describe(before[input]), describe(after[input]))
	}
	for _, input := range inputs {
		if diff := observationDiff(before[input], after[input]); diff != "" {
			fmt.Fprintf(&text, "\n`%s` observation: %s\n", input, diff)
		}
	}
	if v.failed() {
		fmt.Fprintf(&text, "\n### Verification failed\n\n```\n%s\n```\n", strings.TrimSpace(v.err.Error()))
	}
	if len(v.planned.Resources) > 0 {
		text.WriteString("\n### Plan\n\n")
		for _, resource := range v.planned.Resources {
			label := resource.Kind + "/" + resource.Name
			if artifact, ok := resource.Artifacts["installer"]; ok {
				label += " prepares `" + artifact.Filename + "`"
				if artifact.Version != "" {
					label += " (" + artifact.Version + ")"
				}
			}
			fmt.Fprintf(&text, "- %s\n", label)
			if resource.Error != "" && len(resource.Destinations) == 0 {
				fmt.Fprintf(&text, "  - failed: %s\n", firstLine(resource.Error))
			}
			for _, destination := range resource.Destinations {
				switch {
				case destination.Error != "":
					fmt.Fprintf(&text, "  - %s: failed: %s\n", destination.Name, firstLine(destination.Error))
				case len(destination.Changes) == 0:
					fmt.Fprintf(&text, "  - %s: no changes\n", destination.Name)
				default:
					fmt.Fprintf(&text, "  - %s: %s\n", destination.Name, plural(len(destination.Changes), "change"))
					for _, item := range destination.Changes {
						fmt.Fprintf(&text, "    - %s %s\n", item.Action, item.Field)
					}
				}
			}
		}
	}
	text.WriteString("\nMerging applies the reviewed lock offline from the cache this run warmed.\n")
	return text.String()
}

func describe(entry source.Entry) string {
	if entry.Version == 0 {
		return "—"
	}
	digest := entry.Content.Artifact.SHA256
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return fmt.Sprintf("`%s` %s `%s…` %s", entry.Content.Filename, humanize.IBytes(uint64(max(entry.Content.Artifact.Size, 0))), digest, entry.ResolvedAt.Format("2006-01-02"))
}

// observationDiff lists resolver observation fields that changed, without
// interpreting resolver-owned vocabulary.
func observationDiff(before, after source.Entry) string {
	if before.Version == 0 || after.Version == 0 {
		return ""
	}
	var previous, current map[string]any
	if json.Unmarshal(before.Observation, &previous) != nil || json.Unmarshal(after.Observation, &current) != nil {
		return ""
	}
	var parts []string
	for _, key := range slices.Sorted(maps.Keys(current)) {
		if fmt.Sprint(previous[key]) != fmt.Sprint(current[key]) {
			parts = append(parts, fmt.Sprintf("%s %s → %s", key, value(previous[key]), value(current[key])))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(previous)) {
		if _, ok := current[key]; !ok {
			parts = append(parts, fmt.Sprintf("%s %s removed", key, value(previous[key])))
		}
	}
	return strings.Join(parts, ", ")
}

// value renders one observation field, with an absent field as a dash.
func value(v any) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("`%v`", v)
}

// planSummary condenses a verification into a status line.
func planSummary(change change, v verification) string {
	name := change.kind + "/" + change.name
	if v.failed() {
		return "failure: " + firstLine(v.err.Error())
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
