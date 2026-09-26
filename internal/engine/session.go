package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// session holds the project lock and cache lease for one finite execution.
type session struct {
	project config.Project
	// base is the catalog prepare --changed-since compares with.
	base    catalog
	root    string
	store   *cas.Store
	manager *source.Manager
	ops     *operations
	work    string
	closers []func() error
}

// open loads the project, serializes it and leases the cache before any
// operation code runs. Plugins load from their lock entries; resolvePlugins
// locks the tags and local paths whose entries are missing or stale.
func open(ctx context.Context, opts Options, resolvePlugins bool) (_ *session, err error) {
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	s := &session{project: p}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	s.root, err = filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return nil, err
	}
	if opts.ChangedSince != "" {
		if s.base, err = catalogAt(s.root, filepath.Base(opts.ConfigPath), opts.ChangedSince); err != nil {
			return nil, err
		}
		if err := reviewedPlugins(s.root, p, s.base, opts.ChangedSince); err != nil {
			return nil, err
		}
	}
	unlock, err := lockfile.Lock(ctx, s.root)
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, unlock)
	s.store, err = cas.Open(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	release, err := s.store.Lease(ctx)
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, release)
	s.manager = source.New(s.store, s.root, opts.Lock.Offline)
	s.work, err = os.MkdirTemp(filepath.Join(s.store.Dir, "work"), "operations-*")
	if err != nil {
		return nil, err
	}
	work := s.work
	s.closers = append(s.closers, func() error { return os.RemoveAll(work) })
	s.ops, err = loadOperations(ctx, p, s.manager, work, opts.Handlers, resolvePlugins)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// close releases the lease and project lock after operation workspaces are gone.
func (s *session) close() {
	for _, closer := range slices.Backward(s.closers) {
		_ = closer()
	}
}

// declarations selects the inputs the lockfile owns for each resource.
func declarations(plans map[string]resourcePlan, selected []string) map[string]map[string]plugin.Input {
	result := map[string]map[string]plugin.Input{}
	for _, key := range selected {
		result[key] = map[string]plugin.Input{}
		for name, input := range plans[key].Inputs {
			if input.Resource == nil {
				result[key][name] = input
			}
		}
	}
	return result
}

// suspended lists the resources implicit runs skip. Their reviewed
// lock entries outlive every run that does not select them.
func suspended(resources map[string]config.Resource) []string {
	var keys []string
	for _, key := range sortedKeys(resources) {
		if resources[key].Suspend {
			keys = append(keys, key)
		}
	}
	return keys
}
