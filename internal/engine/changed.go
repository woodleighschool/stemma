package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

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
	Identity  string                  `json:"identity"`
	Resolvers map[string]string       `json:"resolvers,omitempty"`
	Config    json.RawMessage         `json:"config"`
	Inputs    map[string]plugin.Input `json:"inputs"`
	Locked    map[string]source.Entry `json:"locked,omitempty"`
}

// catalog is a composed project with its reviewed lockfile.
type catalog struct {
	project config.Project
	lock    lockfile.File
	ops     *operations
	changed []string
}

// catalogAt reads the catalog as it was where the histories of rev and HEAD
// meet. A commit without the catalog declares no resources.
func (s *session) catalogAt(ctx context.Context, opts Options) (_ catalog, err error) {
	rev := opts.ChangedSince
	defer func() {
		if err != nil {
			err = fmt.Errorf("catalog at %s: %w; verify this change with an explicit preparation run", rev, err)
		}
	}()
	repo, err := git.Open(s.root)
	if err != nil {
		return catalog{}, err
	}
	commit, err := repo.MergeBase(rev)
	if err != nil {
		return catalog{}, err
	}
	dir, err := filepath.Rel(repo.Dir, s.root)
	if err != nil {
		return catalog{}, err
	}
	worktree, err := repo.Worktree(commit)
	if err != nil {
		return catalog{}, err
	}
	s.closers = append(s.closers, worktree.Remove)
	base := filepath.Join(worktree.Dir, dir)
	var result catalog
	changed, err := repo.ChangedPaths(ctx, commit)
	if err != nil {
		return catalog{}, err
	}
	for _, name := range changed {
		relative, err := filepath.Rel(dir, filepath.FromSlash(name))
		if err != nil {
			return catalog{}, err
		}
		result.changed = append(result.changed, filepath.ToSlash(relative))
	}
	result.project, err = config.Load(filepath.Join(base, filepath.Base(opts.ConfigPath)))
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err == nil {
		result.lock, err = lockfile.Load(lockfile.Filename(base))
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		return catalog{}, err
	}
	result.ops, err = loadOperations(ctx, result.project, source.New(s.store, base, opts.Lock.Offline), opts.Handlers, false)
	return result, err
}

// changedSince narrows roots to the resources whose preparation differs from
// the catalog at the session's base revision, and the resources that consume
// them. It reads both catalogs as written, so only the resources it returns
// need environment values, and checks the lockfile's shape against the roots
// and everything they consume.
func changedSince(ctx context.Context, s *session, rev string, roots, profiles []string) (_ []string, err error) {
	done := plugin.Stage(ctx, "Comparing with catalog", plugin.Detail(rev))
	defer func() { done(err) }()
	locked, err := lockfile.Load(lockfile.Filename(s.root))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	kinds := resourceKinds(s.ops)
	baseKinds := resourceKinds(s.base.ops)
	current := map[string]preparation{}
	inputs := map[string]map[string]plugin.Input{}
	consumers := map[string][]string{}
	affected := map[string]bool{}
	var read func(string) error
	read = func(key string) error {
		if _, seen := current[key]; seen {
			return nil
		}
		prepared, err := preparationOf(ctx, s.ops, kinds, s.project.Resources[key], locked)
		if err != nil {
			return fmt.Errorf("resource %s: %w", key, err)
		}
		current[key], inputs[key] = prepared, map[string]plugin.Input{}
		for _, name := range sortedKeys(prepared.Inputs) {
			input := prepared.Inputs[name]
			if input.Resource == nil {
				inputs[key][name] = input
				continue
			}
			producer := input.Resource.Key()
			consumers[producer] = append(consumers[producer], key)
			if resource, declared := s.project.Resources[producer]; declared && !resource.Suspend {
				if err := read(producer); err != nil {
					return err
				}
			} else {
				// Keep the consumer selected so normal evaluation rejects the dependency.
				affected[key] = true
			}
		}
		return nil
	}
	for _, key := range roots {
		if err := read(key); err != nil {
			return nil, err
		}
	}
	if err := lockfile.Check(s.root, inputs, unselected(s.project.Resources, sortedKeys(current))); err != nil {
		return nil, err
	}
	for key, prepared := range current {
		previous, declared := s.base.project.Resources[key]
		// A resource the same run would not have taken at the base counts as new.
		if !declared || previous.Suspend || isRoot(s.project.Resources[key], profiles) && !isRoot(previous, profiles) {
			affected[key] = true
			continue
		}
		reviewed, err := preparationOf(ctx, s.base.ops, baseKinds, previous, s.base.lock)
		if err != nil {
			return nil, fmt.Errorf("resource %s at %s: %w", key, rev, err)
		}
		if config.Fingerprint(prepared) != config.Fingerprint(reviewed) {
			affected[key] = true
			continue
		}
		for _, input := range prepared.Inputs {
			if input.Resource != nil {
				continue
			}
			changed, err := s.manager.InputChanged(input, s.base.changed)
			if err != nil {
				return nil, fmt.Errorf("resource %s: %w", key, err)
			}
			if changed {
				affected[key] = true
				break
			}
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
		return preparation{}, ops.missing("no installed operation registers this apiVersion and kind")
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
	prepared := preparation{Operation: op.Name, Identity: ops.identity[op.Name], Resolvers: map[string]string{}, Config: result.Config, Inputs: map[string]plugin.Input{}, Locked: lock.Inputs[r.Reference().Key()]}
	for name, input := range result.Inputs {
		input.Base = r.Base
		prepared.Inputs[name] = input
		if input.Resource == nil && !source.NativeResolver(input.Resolver) {
			resolver, err := ops.lookup(input.Resolver, "resolver")
			if err != nil {
				return preparation{}, err
			}
			if resolver.Resolver == nil {
				return preparation{}, fmt.Errorf("unknown resolver %q", input.Resolver)
			}
			prepared.Resolvers[input.Resolver] = ops.identity[input.Resolver]
		}
	}
	return prepared, nil
}
