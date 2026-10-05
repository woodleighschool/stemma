package engine

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/plugin"
)

// Report distinguishes source, preparation and each destination's work.
type Report struct {
	// LockChanged reports whether update wrote the lockfile.
	LockChanged *bool `json:"lock_changed,omitempty"`
	// RemovedInputs are lock entries of resources the catalog no longer declares.
	RemovedInputs []lockfile.InputChange `json:"removed_inputs,omitempty"`
	// Plugins are the plugin lock entries an update changed.
	Plugins   []lockfile.PluginChange `json:"plugins,omitempty"`
	Warnings  []string                `json:"warnings,omitempty"`
	Summary   Summary                 `json:"summary"`
	Error     string                  `json:"error,omitempty"`
	Resources []ResourceReport        `json:"resources"`
	// Artifact is where the artifact method materialized the selected output.
	Artifact string `json:"artifact,omitempty"`
	// Inspection is the static evidence of the selected resource input.
	Inspection *Inspection `json:"inspection,omitempty"`
}

// ResourceReport separates immutable outputs from destination reconciliation.
type ResourceReport struct {
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Key            string          `json:"key"`
	InputCacheHits map[string]bool `json:"input_cache_hits,omitempty"`
	// Inputs are the lock changes this run commits for the resource, or would
	// commit when it ignores input locks.
	Inputs       []lockfile.InputChange `json:"inputs,omitempty"`
	Artifacts    map[string]Prepared    `json:"artifacts,omitempty"`
	Cached       bool                   `json:"cached"`
	Destinations []DestinationReport    `json:"destinations,omitempty"`
	// Icon reports what the icon method did for the resource's declared asset.
	Icon     string `json:"icon,omitempty"`
	IconPath string `json:"icon_path,omitempty"`
	Error    string `json:"error,omitempty"`
	// BlockedBy names the resources whose unavailable outputs prevented execution.
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// DestinationReport describes semantic drift independently of cache hits.
type DestinationReport struct {
	Artifact string            `json:"artifact,omitempty"`
	Version  string            `json:"version,omitempty"`
	Name     string            `json:"name"`
	Origins  map[string]string `json:"origins,omitempty"`
	Changes  []plugin.Change   `json:"changes"`
	Applied  bool              `json:"reconciled"`
	Error    string            `json:"error,omitempty"`
}

// MarshalJSON keeps preparation cache state out of unrelated command results.
func (r ResourceReport) MarshalJSON() ([]byte, error) {
	type resource ResourceReport
	var cached *bool
	if len(r.Artifacts) > 0 && r.Icon == "" {
		cached = &r.Cached
	}
	return json.Marshal(struct {
		resource

		Cached *bool `json:"cached,omitempty"`
	}{resource: resource(r), Cached: cached})
}

// MarshalJSON distinguishes planned drift, successful reconciliation and failure.
func (r DestinationReport) MarshalJSON() ([]byte, error) {
	type destination DestinationReport
	if r.Changes == nil {
		r.Changes = []plugin.Change{}
	}
	status := "planned"
	switch {
	case r.Error != "":
		status = "failed"
	case len(r.Changes) == 0:
		status = "unchanged"
	case r.Applied:
		status = "applied"
	}
	return json.Marshal(struct {
		destination

		Status string `json:"status"`
	}{destination: destination(r), Status: status})
}

// ResourceError identifies a resource failure independently of command-wide failures.
type ResourceError struct {
	Resource string
	Err      error
}

func (e ResourceError) Error() string { return e.Resource + ": " + e.Err.Error() }
func (e ResourceError) Unwrap() error { return e.Err }

// Unreported is the part of a run's error that its report does not carry with
// a resource, or nil. Joined errors are split; a wrapped error keeps its
// context whole.
func Unreported(err error) error {
	if _, ok := err.(ResourceError); ok { //nolint:errorlint // A wrapped resource failure has context the report lacks.
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var rest []error
	for _, child := range joined.Unwrap() {
		if child := Unreported(child); child != nil {
			rest = append(rest, child)
		}
	}
	return errors.Join(rest...)
}

// Summary counts the entire run, including resources omitted from its presentation.
type Summary struct {
	method                string
	Resources             int `json:"resources"`
	InputChanges          int `json:"input_changes,omitempty"`
	Changed               int `json:"changed,omitempty"`
	Unchanged             int `json:"unchanged,omitempty"`
	Failed                int `json:"failed,omitempty"`
	Blocked               int `json:"blocked,omitempty"`
	Skipped               int `json:"skipped,omitempty"`
	Destinations          int `json:"destinations,omitempty"`
	DestinationOperations int `json:"destination_operations,omitempty"`
	Applied               int `json:"applied,omitempty"`
	Resolved              int `json:"resolved,omitempty"`
	Prepared              int `json:"prepared,omitempty"`
	Cached                int `json:"cached,omitempty"`
	Created               int `json:"created,omitempty"`
	Derived               int `json:"derived,omitempty"`
}

// Summarize records totals before any presentation filter is applied.
func (r *Report) Summarize(method string) {
	s := Summary{method: method, Resources: len(r.Resources), InputChanges: len(r.RemovedInputs)}
	destinations := map[string]bool{}
	for _, resource := range r.Resources {
		s.InputChanges += len(resource.Inputs)
		switch {
		case len(resource.BlockedBy) > 0:
			s.Blocked++
		case resource.Error != "":
			s.Failed++
		case method == "update":
			s.Resolved++
		case method == "icon":
			switch {
			case strings.HasPrefix(resource.Icon, "created "):
				s.Created++
			case resource.Icon == "unchanged":
			default:
				s.Skipped++
			}
		case method == "signature":
			if derivedSignatures(resource) {
				s.Derived++
			}
		case resource.Cached:
			s.Cached++
		default:
			s.Prepared++
		}
		changed := len(resource.Inputs) > 0
		for _, destination := range resource.Destinations {
			s.DestinationOperations++
			destinations[destination.Name] = true
			if len(destination.Changes) > 0 {
				changed = true
				if destination.Applied {
					s.Applied++
				}
			}
		}
		if method == "icon" {
			changed = strings.HasPrefix(resource.Icon, "created ")
		}
		if changed {
			s.Changed++
		} else if resource.Error == "" && len(resource.BlockedBy) == 0 && (method != "icon" || resource.Icon == "unchanged") {
			s.Unchanged++
		}
	}
	s.Destinations = len(destinations)
	r.Summary = s
}

// derivedSignatures reports whether derivation observed a signing subject of
// the resource. A package its builder left unsigned has none to declare.
func derivedSignatures(resource ResourceReport) bool {
	for _, artifact := range resource.Artifacts {
		var observations []json.RawMessage
		if json.Unmarshal(artifact.Evidence["signatures"], &observations) == nil && len(observations) > 0 {
			return true
		}
	}
	return false
}

// MarshalJSON exposes predictable counters relevant to the command that ran.
func (s Summary) MarshalJSON() ([]byte, error) {
	counts := map[string]int{"resources": s.Resources, "failed": s.Failed, "blocked": s.Blocked}
	switch s.method {
	case "":
		type summary Summary
		return json.Marshal(summary(s))
	case "icon":
		counts["created"], counts["unchanged"], counts["skipped"] = s.Created, s.Unchanged, s.Skipped
	case "update":
		counts["resolved"], counts["input_changes"], counts["unchanged"] = s.Resolved, s.InputChanges, s.Unchanged
	case "signature":
		counts["derived"] = s.Derived
	default:
		counts["prepared"], counts["cached"] = s.Prepared, s.Cached
		if s.method == "plan" || s.method == "apply" {
			counts["changed"], counts["unchanged"] = s.Changed, s.Unchanged
			counts["destinations"], counts["destination_operations"] = s.Destinations, s.DestinationOperations
			if s.method == "apply" {
				counts["applied"] = s.Applied
			}
		}
	}
	return json.Marshal(counts)
}
