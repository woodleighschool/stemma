package lockfile

import (
	"maps"
	"slices"

	"github.com/woodleighschool/stemma/internal/plugins"
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

// PluginChange compares a plugin's reviewed lock entry with the one a run
// records. An entry appears when a tag or local path is locked and disappears
// when its plugin is no longer declared or its declaration names a digest.
type PluginChange struct {
	Name   string         `json:"name"`
	Before *plugins.Entry `json:"before,omitempty"`
	After  *plugins.Entry `json:"after,omitempty"`
}

// DiffPlugins lists added, changed and removed plugin entries by name.
func DiffPlugins(before, after map[string]plugins.Entry) []PluginChange {
	names := slices.Collect(maps.Keys(before))
	for name := range after {
		if _, ok := before[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var changes []PluginChange
	for _, name := range names {
		a, aok := before[name]
		b, bok := after[name]
		if aok && bok && a == b {
			continue
		}
		change := PluginChange{Name: name}
		if aok {
			change.Before = &a
		}
		if bok {
			change.After = &b
		}
		changes = append(changes, change)
	}
	return changes
}
