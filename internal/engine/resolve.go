package engine

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Candidate is one whole-catalog resolution held in memory. Callers decide
// which resources become lockfile changes; Resolve never writes the lockfile.
type Candidate struct {
	// Lock is the reviewed lockfile the catalog was resolved against.
	Lock lockfile.File `json:"lock"`
	// Plugins are the locked plugin entries whose code performed the resolution.
	Plugins   map[string]plugins.Entry     `json:"plugins,omitempty"`
	Resources map[string]CandidateResource `json:"resources"`
}

// CandidateResource reports one resource's current inputs, or why they could
// not be observed, alongside the resources whose outputs it consumes.
type CandidateResource struct {
	Name      string                  `json:"name"`
	Kind      string                  `json:"kind"`
	Inputs    map[string]source.Entry `json:"inputs,omitempty"`
	Producers []string                `json:"producers,omitempty"`
	// Suspended marks a resource left unresolved on purpose. Its lock entries
	// stay as they are and nothing implicit runs it.
	Suspended bool   `json:"suspended,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Dependents returns every resource that transitively consumes key's outputs.
// Suspended consumers are left out: nothing implicit ever runs them.
func (c Candidate) Dependents(key string) []string {
	consumers := map[string][]string{}
	for name, resource := range c.Resources {
		if resource.Suspended {
			continue
		}
		for _, producer := range resource.Producers {
			consumers[producer] = append(consumers[producer], name)
		}
	}
	seen := map[string]bool{}
	var visit func(string)
	visit = func(key string) {
		for _, consumer := range consumers[key] {
			if !seen[consumer] {
				seen[consumer] = true
				visit(consumer)
			}
		}
	}
	visit(key)
	return slices.Sorted(maps.Keys(seen))
}

// Resolve observes every declared input of the catalog once and reports each
// resource independently, so one unreachable source cannot hide the others.
// Plugins must match the lockfile: their code runs before anything is observed.
func Resolve(ctx context.Context, opts Options) (candidate Candidate, runErr error) {
	opts.Lock = lockfile.Options{Refresh: true, Hints: opts.Lock.Hints}
	s, err := open(ctx, opts, true)
	if err != nil {
		return candidate, err
	}
	defer s.close()
	done := plugin.Stage(ctx, "Validating operation contracts")
	defer func() { done(runErr) }()
	plans, err := discover(ctx, s.project, s.ops)
	if err != nil {
		return candidate, err
	}
	selected, err := orderResources(plans, nil)
	if err != nil {
		return candidate, err
	}
	if err := preflight(plans, selected, s.project, s.ops); err != nil {
		return candidate, err
	}
	if err := registerResolvers(s.manager, s.ops, s.work); err != nil {
		return candidate, err
	}
	locked, err := lockfile.Begin(ctx, s.root, declarations(plans, selected), s.ops.plugins, s.manager, opts.Lock)
	if err != nil {
		return candidate, err
	}
	candidate.Lock, err = lockfile.Load(lockfile.Filename(s.root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return candidate, err
	}
	done(nil)
	candidate.Plugins = s.ops.plugins
	candidate.Resources = map[string]CandidateResource{}
	for _, key := range selected {
		plan := plans[key]
		resource := CandidateResource{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Producers: producers(plan)}
		ctx := resourceContext(ctx, plan.Resource)
		entries, _, err := locked.Acquire(ctx, key)
		if err != nil {
			if ctx.Err() != nil {
				return candidate, err
			}
			plugin.Logger(ctx).DebugContext(ctx, "Resolution failed", "error", err)
			resource.Error = err.Error()
		} else if len(entries) > 0 {
			resource.Inputs = entries
		}
		candidate.Resources[key] = resource
	}
	for _, key := range suspended(plans) {
		plan := plans[key]
		candidate.Resources[key] = CandidateResource{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Producers: producers(plan), Suspended: true}
	}
	return candidate, nil
}

// producers lists the resources whose outputs a plan consumes.
func producers(plan resourcePlan) []string {
	var keys []string
	for _, name := range sortedKeys(plan.Inputs) {
		if ref := plan.Inputs[name].Resource; ref != nil {
			keys = append(keys, ref.Key())
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}
