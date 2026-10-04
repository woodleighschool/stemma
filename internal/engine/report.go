package engine

import (
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
	Name    string            `json:"name"`
	Origins map[string]string `json:"origins,omitempty"`
	Changes []plugin.Change   `json:"changes"`
	Applied bool              `json:"applied"`
	Error   string            `json:"error,omitempty"`
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
	s := Summary{Resources: len(r.Resources), InputChanges: len(r.RemovedInputs)}
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
			s.Derived++
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
