package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/git"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// preparation is what preparing a resource means in the catalog: the
// preparation settings its kind reads from the declaration as written, the
// inputs it declares and their reviewed entries.
type preparation struct {
	Operation string                  `json:"operation"`
	Config    json.RawMessage         `json:"config"`
	Inputs    map[string]plugin.Input `json:"inputs"`
	Locked    map[string]source.Entry `json:"locked,omitempty"`
}

// catalog is a composed project with its reviewed lockfile.
type catalog struct {
	project config.Project
	lock    lockfile.File
}

// catalogAt reads the catalog as it was where the histories of rev and HEAD
// meet. A commit without the catalog declares no resources.
func catalogAt(root, name, rev string) (catalog, error) {
	repo, err := git.Open(root)
	if err != nil {
		return catalog{}, err
	}
	commit, err := repo.MergeBase(rev)
	if err != nil {
		return catalog{}, err
	}
	dir, err := filepath.Rel(repo.Dir, root)
	if err != nil {
		return catalog{}, err
	}
	worktree, err := repo.Worktree(commit)
	if err != nil {
		return catalog{}, err
	}
	defer func() { _ = worktree.Remove() }()
	base := filepath.Join(worktree.Dir, dir)
	var result catalog
	result.project, err = config.Load(filepath.Join(base, name))
	if errors.Is(err, fs.ErrNotExist) {
		return catalog{}, nil
	}
	if err == nil {
		result.lock, err = lockfile.Load(lockfile.Filename(base))
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		return catalog{}, fmt.Errorf("catalog at %s: %w; a change this Stemma cannot compare requires trusted verification", rev, err)
	}
	return result, nil
}

// reviewedPlugins fails when the plugins the catalog declares or locks differ
// from those at rev. It runs before any plugin loads: plugin code runs with
// the runner's privileges, so a new plugin needs trusted verification.
func reviewedPlugins(root string, p config.Project, base catalog, rev string) error {
	locked, err := lockfile.Load(lockfile.Filename(root))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	names := slices.Concat(slices.Collect(maps.Keys(p.Plugins)), slices.Collect(maps.Keys(base.project.Plugins)), slices.Collect(maps.Keys(locked.Plugins)), slices.Collect(maps.Keys(base.lock.Plugins)))
	slices.Sort(names)
	var changed []string
	for _, name := range slices.Compact(names) {
		if p.Plugins[name] != base.project.Plugins[name] || config.Fingerprint(locked.Plugins[name]) != config.Fingerprint(base.lock.Plugins[name]) {
			changed = append(changed, name)
		}
	}
	if len(changed) > 0 {
		return fmt.Errorf("plugin %s changed since %s; plugin changes require trusted verification", strings.Join(changed, ", "), rev)
	}
	return nil
}

// changedSince returns the resources whose preparation differs from the
// catalog at the session's base revision, and the resources that consume
// them. It reads both catalogs as written, so only the resources it returns
// need environment values, and checks the lockfile's shape against the whole
// catalog.
func changedSince(ctx context.Context, s *session, rev string) (roots []string, err error) {
	done := plugin.Stage(ctx, "Comparing with catalog", plugin.Detail(rev))
	defer func() { done(err) }()
	locked, err := lockfile.Load(lockfile.Filename(s.root))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	kinds := resourceKinds(s.ops)
	current := map[string]preparation{}
	inputs := map[string]map[string]plugin.Input{}
	consumers := map[string][]string{}
	for _, key := range sortedKeys(s.project.Resources) {
		if s.project.Resources[key].Suspend {
			continue
		}
		prepared, err := preparationOf(ctx, s.ops, kinds, s.project.Resources[key], locked)
		if err != nil {
			return nil, fmt.Errorf("resource %s: %w", key, err)
		}
		current[key], inputs[key] = prepared, map[string]plugin.Input{}
		for name, input := range prepared.Inputs {
			if input.Resource == nil {
				inputs[key][name] = input
			} else {
				consumers[input.Resource.Key()] = append(consumers[input.Resource.Key()], key)
			}
		}
	}
	if err := lockfile.Check(s.root, inputs, suspended(s.project.Resources)); err != nil {
		return nil, err
	}
	affected := map[string]bool{}
	for key, prepared := range current {
		previous, declared := s.base.project.Resources[key]
		if !declared || previous.Suspend {
			affected[key] = true
			continue
		}
		// A declaration the current kinds cannot read is a change.
		reviewed, err := preparationOf(ctx, s.ops, kinds, previous, s.base.lock)
		if err != nil || config.Fingerprint(prepared) != config.Fingerprint(reviewed) {
			affected[key] = true
		}
	}
	for queue := sortedKeys(affected); len(queue) > 0; queue = queue[1:] {
		for _, consumer := range consumers[queue[0]] {
			if !affected[consumer] {
				affected[consumer] = true
				queue = append(queue, consumer)
			}
		}
	}
	return sortedKeys(affected), nil
}

// preparationOf reads a resource's preparation as written: environment values
// are the same on both sides of a comparison, and destination metadata never
// shapes what is prepared.
func preparationOf(ctx context.Context, ops *operations, kinds map[plugin.ResourceKind]plugin.Operation, r config.Resource, lock lockfile.File) (preparation, error) {
	op, ok := kinds[plugin.ResourceKind{APIVersion: r.APIVersion, Kind: r.Kind}]
	if !ok {
		return preparation{}, errors.New("no installed operation registers this apiVersion and kind")
	}
	declaration, _, err := resourceDeclaration(r, false)
	if err != nil {
		return preparation{}, err
	}
	encoded, err := json.Marshal(declaration)
	if err != nil {
		return preparation{}, err
	}
	var result plugin.ResourceResult
	if err := ops.call(ctx, op.Name, "discover", plugin.ResourceRequest[json.RawMessage]{Config: encoded, Identity: r.Reference()}, &result); err != nil {
		return preparation{}, err
	}
	prepared := preparation{Operation: op.Name, Config: result.Config, Inputs: map[string]plugin.Input{}, Locked: lock.Inputs[r.Reference().Key()]}
	for name, input := range result.Inputs {
		input.Base = r.Base
		prepared.Inputs[name] = input
	}
	return prepared, nil
}
