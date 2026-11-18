package engine

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

// Report is one command's outcome: what happened to each resource it
// selected, as every surface renders it.
type Report struct {
	Command   string           `json:"command"`
	Summary   Summary          `json:"summary"`
	Resources []ResourceReport `json:"resources"`
	// Plugins are the plugin lock entries an update changed.
	Plugins []lock.PluginChange `json:"plugins,omitempty"`
	// Removed are lock entries of resources the catalog no longer declares.
	Removed []lock.InputChange `json:"removed,omitempty"`
	// LockChanged reports whether update wrote the lock.
	LockChanged *bool    `json:"lock_changed,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Error       string   `json:"error,omitempty"`
	// Artifact is where the artifact method materialized the selected output.
	Artifact string `json:"artifact,omitempty"`
	// Inspection is the static evidence of the selected resource input.
	Inspection *Inspection `json:"inspection,omitempty"`
}

// Status is what a command did to a resource or a publication.
type Status string

const (
	// StatusChanged is the command's work done: inputs updated, an artifact
	// prepared, an icon created, signers derived, a publication planned or applied.
	StatusChanged Status = "changed"
	// StatusUnchanged is nothing to do: inputs current, the artifact cached,
	// the icon present, the destination current.
	StatusUnchanged Status = "unchanged"
	StatusFailed    Status = "failed"
	// StatusBlocked means a required output or publication could not complete.
	StatusBlocked Status = "blocked"
	// StatusSkipped is a resource the command does not apply to; Reason says why.
	StatusSkipped Status = "skipped"
)

func (s Status) unsuccessful() bool {
	return s == StatusFailed || s == StatusBlocked
}

// ResourceReport is one resource's outcome.
type ResourceReport struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
	// Inputs are the lock changes this run commits for the resource, or would
	// commit when it ignores input locks.
	Inputs       []lock.InputChange  `json:"inputs,omitempty"`
	Artifacts    map[string]Prepared `json:"artifacts,omitempty"`
	Publications []Publication       `json:"publications,omitempty"`
	// Icon is the asset path the icon method created or found.
	Icon      string          `json:"icon,omitempty"`
	Notices   []plugin.Notice `json:"notices,omitempty"`
	Error     string          `json:"error,omitempty"`
	BlockedBy Blockers        `json:"blocked_by,omitempty"`
}

// Blockers lists the resource keys required at the point work stopped.
type Blockers []string

// Names returns each blocker's Kind/name for display.
func (b Blockers) Names() []string {
	names := make([]string, len(b))
	for i, key := range b {
		parts := strings.Split(key, "/")
		names[i] = strings.Join(parts[max(0, len(parts)-2):], "/")
	}
	return names
}

// Publication is a resource's publication to one destination. Preparation
// previews its managed Metadata without reading the destination, and
// Deferred says why a preview is absent; plan and apply add what the
// destination reported.
type Publication struct {
	Destination string               `json:"destination"`
	Status      Status               `json:"status,omitempty"`
	Version     string               `json:"version,omitempty"`
	Previous    string               `json:"previous,omitempty"`
	Uploads     []plugin.Upload      `json:"uploads,omitempty"`
	Fields      []plugin.FieldChange `json:"fields,omitempty"`
	Repairs     []string             `json:"repairs,omitempty"`
	Removed     []string             `json:"removed,omitempty"`
	Metadata    json.RawMessage      `json:"metadata,omitempty"`
	Origins     map[string]string    `json:"origins,omitempty"`
	Deferred    string               `json:"deferred,omitempty"`
	BlockedBy   Blockers             `json:"blocked_by,omitempty"`
	Error       string               `json:"error,omitempty"`
}

// signatureNotices returns the advisories that the signing evidence of a
// resource's outputs raises. The evidence is stored with each output, so they
// are the same when preparation is cached.
func signatureNotices(resource plugin.ResourceReference, artifacts map[string]Prepared) []plugin.Notice {
	var notices []plugin.Notice
	for _, name := range slices.Sorted(maps.Keys(artifacts)) {
		notices = append(notices, signature.Notices(resource, artifacts[name].Evidence)...)
	}
	return notices
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

// Summary counts the resources a run selected by status, including those its
// presentation omits. NotReached are the selected resources a stopped run
// never finished; Removed counts what retention deletes.
type Summary struct {
	Resources  int `json:"resources"`
	Changed    int `json:"changed"`
	Unchanged  int `json:"unchanged"`
	Failed     int `json:"failed"`
	Blocked    int `json:"blocked"`
	Skipped    int `json:"skipped"`
	NotReached int `json:"not_reached"`
	Removed    int `json:"removed"`
}

// Summarize counts the report's resources against the number the run selected.
func (r *Report) Summarize(selected int) {
	s := Summary{Resources: max(selected, len(r.Resources))}
	for _, resource := range r.Resources {
		switch resource.Status {
		case StatusChanged:
			s.Changed++
		case StatusUnchanged:
			s.Unchanged++
		case StatusFailed:
			s.Failed++
		case StatusBlocked:
			s.Blocked++
		case StatusSkipped:
			s.Skipped++
		}
		for _, publication := range resource.Publications {
			s.Removed += len(publication.Removed)
		}
	}
	s.NotReached = s.Resources - len(r.Resources)
	r.Summary = s
}

// status settles a finished resource's status from what the command did.
func (r *ResourceReport) status(method string) {
	switch {
	case len(r.BlockedBy) > 0:
		r.Status = StatusBlocked
		return
	case r.Error != "":
		r.Status = StatusFailed
		return
	case r.Status != "":
		// The icon method settles its own outcome.
		return
	}
	r.Status = StatusUnchanged
	switch method {
	case "update":
		if len(r.Inputs) > 0 {
			r.Status = StatusChanged
		}
	case "signature":
		r.Status = StatusSkipped
		r.Reason = "nothing to declare"
		for _, artifact := range r.Artifacts {
			if len(signature.Read(artifact.Evidence).Observed) > 0 {
				r.Status, r.Reason = StatusChanged, ""
			}
		}
	case "icon":
		// The icon method settled its targets above; this resource was only
		// prepared for another's icon.
	case "plan", "apply":
		for _, publication := range r.Publications {
			if publication.Status == StatusBlocked {
				r.Status = StatusBlocked
				return
			}
			if publication.Status == StatusChanged {
				r.Status = StatusChanged
			}
		}
	default:
		for _, artifact := range r.Artifacts {
			if !artifact.Cached {
				r.Status = StatusChanged
			}
		}
	}
}
