package engine

import "strings"

// Summary counts the entire run, including resources omitted from its presentation.
type Summary struct {
	Resources    int `json:"resources"`
	InputChanges int `json:"input_changes"`
	Changed      int `json:"changed"`
	Unchanged    int `json:"unchanged"`
	Failed       int `json:"failed"`
	Blocked      int `json:"blocked"`
	Destinations int `json:"destinations"`
	Applied      int `json:"applied"`
	Prepared     int `json:"prepared"`
	Cached       int `json:"cached"`
}

// Summarize records totals before any presentation filter is applied.
func (r *Report) Summarize(method string) {
	s := Summary{Resources: len(r.Resources), InputChanges: len(r.RemovedInputs)}
	for _, resource := range r.Resources {
		s.InputChanges += len(resource.Inputs)
		switch {
		case len(resource.BlockedBy) > 0:
			s.Blocked++
		case resource.Error != "":
			s.Failed++
		case resource.Cached:
			s.Cached++
		default:
			s.Prepared++
		}
		changed := len(resource.Inputs) > 0
		for _, destination := range resource.Destinations {
			if len(destination.Changes) > 0 {
				changed = true
				s.Destinations++
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
		} else if resource.Error == "" {
			s.Unchanged++
		}
	}
	r.Summary = s
}
