package lockfile

import (
	"maps"
	"slices"

	"github.com/woodleighschool/stemma/internal/source"
)

// InputChange compares a reviewed input with the entry committed by this run.
type InputChange struct {
	Resource       string        `json:"resource"`
	Input          string        `json:"input"`
	Before         *source.Entry `json:"before,omitempty"`
	After          *source.Entry `json:"after,omitempty"`
	ContentChanged bool          `json:"content_changed"`
}

// DiffInputs includes additions and removals, ordered by resource and input.
func DiffInputs(before, after map[string]map[string]source.Entry) []InputChange {
	resources := maps.Clone(before)
	if resources == nil {
		resources = map[string]map[string]source.Entry{}
	}
	maps.Copy(resources, after)
	var changes []InputChange
	for _, resource := range slices.Sorted(maps.Keys(resources)) {
		inputs := maps.Clone(before[resource])
		if inputs == nil {
			inputs = map[string]source.Entry{}
		}
		maps.Copy(inputs, after[resource])
		for _, name := range slices.Sorted(maps.Keys(inputs)) {
			a, aok := before[resource][name]
			b, bok := after[resource][name]
			if aok && bok && a.Equal(b) {
				continue
			}
			change := InputChange{Resource: resource, Input: name, ContentChanged: !aok || !bok || a.Content.Artifact != b.Content.Artifact}
			if aok {
				change.Before = &a
			}
			if bok {
				change.After = &b
			}
			changes = append(changes, change)
		}
	}
	return changes
}
