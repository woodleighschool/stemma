package cas

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/plugin"
)

// DefaultMaxSize is the soft retained-cache budget. Workspaces are additional.
const DefaultMaxSize int64 = 32 << 30

// Grace protects recently used entries even when the working set exceeds the budget.
const Grace = 24 * time.Hour

// Policy belongs to the runner, not the catalog. Zero MaxSize disables eviction.
type Policy struct{ MaxSize int64 }

// Usage counts logical file bytes, including metadata and materialized copies.
// Work is reported separately because it is not retained content.
type Usage struct {
	Retained     int64 `json:"retained_bytes"`
	Objects      int64 `json:"object_bytes"`
	Materialized int64 `json:"materialized_bytes"`
	Metadata     int64 `json:"metadata_bytes"`
	Work         int64 `json:"temporary_bytes"`
}

// PruneOptions controls explicit pruning. Automatic collection always tries once.
type PruneOptions struct {
	All    bool
	DryRun bool
}

// Collection reports completed removals, or proposed removals for a dry run.
type Collection struct {
	Before    Usage `json:"before"`
	After     Usage `json:"after"`
	Reclaimed int64 `json:"reclaimed_bytes"`
	Removed   int   `json:"removed_entries"`
}

type recencyKey struct{}

type entry struct {
	id        string
	paths     []string
	usage     Usage
	used      time.Time
	objects   []string
	invalid   bool
	removed   bool
	temporary bool
}

func (u *Usage) add(v Usage, sign int64) {
	u.Retained += sign * v.Retained
	u.Objects += sign * v.Objects
	u.Materialized += sign * v.Materialized
	u.Metadata += sign * v.Metadata
	u.Work += sign * v.Work
}

func (s *Store) touch(ctx context.Context, dir, key string) {
	path := filepath.Join(s.Dir, "uses", dir+"-"+key)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		now := s.now()
		err = os.Chtimes(path, now, now)
	}
	if err != nil {
		if failed, ok := ctx.Value(recencyKey{}).(*atomic.Bool); ok {
			failed.Store(true)
		}
		plugin.Logger(ctx).WarnContext(ctx, "Cache recency could not be recorded", "error", err)
	}
}

// Info inventories the cache without updating recency. A lease excludes pruning;
// concurrent commands may still add content while the snapshot is being read.
func (s *Store) Info(ctx context.Context) (Usage, error) {
	release, err := s.Lease(ctx)
	if err != nil {
		return Usage{}, err
	}
	defer func() { _ = release() }()
	_, usage, err := s.inventory(ctx)
	return usage, err
}

// Prune waits for active runs, then applies the policy without following symlinks.
func (s *Store) Prune(ctx context.Context, policy Policy, opts PruneOptions) (Collection, error) {
	return s.collect(ctx, policy, opts, true, true)
}

func (s *Store) collect(ctx context.Context, policy Policy, opts PruneOptions, wait, evict bool) (Collection, error) {
	l := flock.New(filepath.Join(s.Dir, "cache.lock"))
	defer func() { _ = l.Close() }()
	ok, err := l.TryLock()
	if err == nil && !ok && wait {
		done := plugin.Stage(ctx, "Waiting for active runs")
		ok, err = l.TryLockContext(ctx, 50*time.Millisecond)
		done(err)
	}
	if err != nil {
		return Collection{}, err
	}
	if !ok {
		return Collection{}, ctx.Err()
	}
	entries, usage, err := s.inventory(ctx)
	result := Collection{Before: usage, After: usage}
	if err != nil {
		return result, err
	}
	dependents := map[string][]*entry{}
	for _, e := range entries {
		for _, digest := range e.objects {
			dependents[digest] = append(dependents[digest], e)
		}
	}
	var remove func(*entry) error
	remove = func(e *entry) error {
		if e.removed {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !opts.DryRun {
			for _, path := range e.paths {
				if err := os.RemoveAll(filepath.Join(s.Dir, path)); err != nil {
					return err
				}
			}
		}
		e.removed = true
		result.After.add(e.usage, -1)
		result.Reclaimed += e.usage.Retained + e.usage.Work
		result.Removed++
		if digest, ok := strings.CutPrefix(e.id, "objects-"); ok {
			for _, dependent := range dependents[digest] {
				if err := remove(dependent); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// Exclusive ownership proves all remaining work and partial writes abandoned.
	for _, e := range entries {
		if e.invalid && (evict || e.temporary) || opts.All {
			if err := remove(e); err != nil {
				return result, err
			}
		}
	}
	if !evict || policy.MaxSize == 0 || opts.All {
		return result, nil
	}
	slices.SortFunc(entries, func(a, b *entry) int {
		return cmp.Or(a.used.Compare(b.used), strings.Compare(a.id, b.id))
	})
	cutoff := s.now().Add(-Grace)
	for _, e := range entries {
		if result.After.Retained <= policy.MaxSize {
			break
		}
		if e.removed || !e.used.Before(cutoff) {
			continue
		}
		if err := remove(e); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Store) inventory(ctx context.Context) ([]*entry, Usage, error) {
	groups := map[string]*entry{}
	var usage Usage
	for _, dir := range dirs {
		base := filepath.Join(s.Dir, dir)
		info, err := os.Lstat(base)
		if err != nil {
			return nil, usage, err
		}
		if !info.IsDir() {
			return nil, usage, fmt.Errorf("cache directory %s is not a directory", dir)
		}
		children, err := os.ReadDir(base)
		if err != nil {
			return nil, usage, err
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return nil, usage, err
			}
			name := child.Name()
			path := filepath.Join(dir, name)
			size, err := treeSize(ctx, filepath.Join(s.Dir, path))
			if err != nil {
				return nil, usage, err
			}
			id := dir + "-" + name
			if dir == "materialized" {
				id = "objects-" + name
			}
			if dir == "uses" {
				id = name
			}
			e := groups[id]
			if e == nil {
				e = &entry{id: id}
				groups[id] = e
			}
			e.paths = append(e.paths, path)
			partial := dir == "work" || strings.HasPrefix(name, ".import-") || strings.HasPrefix(name, ".stemma-")
			if partial {
				e.temporary = true
				e.invalid = true
				e.usage.Work += size
				continue
			}
			switch dir {
			case "objects":
				e.usage.Objects += size
			case "materialized":
				e.usage.Materialized += size
			default:
				e.usage.Metadata += size
			}
			e.usage.Retained += size
			if dir == "uses" {
				if info, err := child.Info(); err == nil && info.Mode().IsRegular() {
					e.used = info.ModTime()
				}
				continue
			}
			if !validDigest(name) || dir != "materialized" && !child.Type().IsRegular() || dir == "materialized" && !child.IsDir() {
				e.invalid = true
			}
			if (dir == "sources" || dir == "derivations") && !e.invalid {
				var index index
				data, err := os.ReadFile(filepath.Join(s.Dir, path))
				if err != nil {
					return nil, usage, err
				}
				e.invalid = json.Unmarshal(data, &index) != nil || len(index.Data) == 0
				e.objects = index.Objects
				for _, digest := range index.Objects {
					if !s.HasDigest(digest) {
						e.invalid = true
					}
				}
			}
		}
	}
	entries := make([]*entry, 0, len(groups))
	for _, e := range groups {
		// A recency marker alone is an interrupted or previously evicted entry.
		if len(e.paths) == 1 && strings.HasPrefix(e.paths[0], "uses"+string(filepath.Separator)) {
			e.invalid = true
		}
		usage.add(e.usage, 1)
		entries = append(entries, e)
	}
	return entries, usage, nil
}

func treeSize(ctx context.Context, path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}
