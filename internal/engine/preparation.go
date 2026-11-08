package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

type preparationTask struct {
	item    ResourceReport
	result  preparedResource
	inputs  map[string]Prepared
	failure error
	started time.Time
}

// prepare prepares a resource once, after its producers. Specialised commands
// use this path to acquire only the outputs they consume.
func (e *execution) prepare(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, exists := e.prepared[key]; exists {
		return nil
	}
	for _, producer := range producers(e.plans[key]) {
		if err := e.prepare(ctx, producer); err != nil {
			return err
		}
	}
	p := e.acquire(ctx, key)
	e.build(ctx, p)
	return e.recordPreparation(ctx, p)
}

// prepareAll keeps acquisition, lock updates and reporting on the coordinator.
// Workers only prepare immutable inputs. All workers finish before the session
// releases its workspaces and cache lease; destinations run afterwards.
func (e *execution) prepareAll(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	defer func() {
		// Completion order is useful for progress, while the final report follows
		// dependency order regardless of which build finished first.
		rank := map[string]int{}
		for i, key := range e.selected {
			rank[key] = i
		}
		slices.SortFunc(e.report.Resources, func(a, b ResourceReport) int { return rank[a.Key] - rank[b.Key] })
		for i, item := range e.report.Resources {
			prepared := e.prepared[item.Key]
			prepared.report = i
			e.prepared[item.Key] = prepared
		}
	}()
	limit := min(e.opts.Jobs, len(e.selected))
	finished := make(chan *preparationTask, limit)
	remaining := slices.Clone(e.selected)
	active := 0
	for len(remaining) > 0 || active > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := 0; i < len(remaining) && active < limit; {
			key := remaining[i]
			ready := true
			for _, producer := range producers(e.plans[key]) {
				if _, done := e.prepared[producer]; !done {
					ready = false
					break
				}
			}
			if !ready {
				i++
				continue
			}
			remaining = slices.Delete(remaining, i, i+1)
			p := e.acquire(ctx, key)
			if p.failure != nil {
				if err := e.recordPreparation(ctx, p); err != nil {
					return err
				}
				continue
			}
			active++
			workers.Go(func() {
				e.build(ctx, p)
				finished <- p
			})
		}
		if active == 0 {
			continue
		}
		select {
		case p := <-finished:
			active--
			if err := e.recordPreparation(ctx, p); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (e *execution) acquire(ctx context.Context, key string) *preparationTask {
	plan := e.plans[key]
	ctx = resourceContext(ctx, plan.Resource)
	plugin.Logger(ctx).DebugContext(ctx, "Executing resource")
	p := &preparationTask{item: ResourceReport{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Key: key}, started: time.Now()}
	for _, producer := range producers(plan) {
		if !e.prepared[producer].ready {
			p.item.BlockedBy = append(p.item.BlockedBy, producer)
		}
	}
	if len(p.item.BlockedBy) > 0 {
		p.failure = fmt.Errorf("blocked by %s", strings.Join(p.item.BlockedBy, ", "))
		return p
	}
	acquireCtx := ctx
	if e.opts.Method != "update" {
		acquireCtx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("phase", "Acquire"))
	}
	var entries map[string]source.Entry
	entries, p.item.InputCacheHits, p.failure = e.locked.Acquire(acquireCtx, key)
	if p.failure == nil && e.opts.Method != "update" {
		p.inputs, p.failure = e.inputs(plan, entries)
	}
	return p
}

func (e *execution) build(ctx context.Context, p *preparationTask) {
	if p.failure != nil || e.opts.Method == "update" {
		return
	}
	plan := e.plans[p.item.Key]
	ctx = resourceContext(ctx, plan.Resource)
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("phase", "Prepare"))
	p.result.work = filepath.Join(e.session.work, "resource-"+config.Fingerprint(p.item.Key))
	derive := ""
	if e.opts.Method == "signature" {
		derive = "signature"
	}
	p.result.outputs, p.item.Cached, p.failure = prepareResource(ctx, e.session.store, e.session.ops, plan, p.inputs, p.result.work, derive)
}

func (e *execution) recordPreparation(ctx context.Context, p *preparationTask) error {
	key := p.item.Key
	ctx = resourceContext(ctx, e.plans[key].Resource)
	p.item.Artifacts = p.result.outputs
	p.result.ready = p.failure == nil
	if p.failure != nil {
		p.item.Error = p.failure.Error()
		if len(p.item.BlockedBy) == 0 {
			e.fail(ctx, ResourceError{Resource: key, Err: p.failure})
		}
	}
	p.result.report = len(e.report.Resources)
	e.prepared[key] = p.result
	e.report.Resources = append(e.report.Resources, p.item)
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case p.failure == nil && e.opts.Method == "update":
		plugin.Logger(ctx).DebugContext(ctx, "Inputs resolved", "elapsed", time.Since(p.started).Round(time.Millisecond))
	case p.failure == nil:
		artifact := p.result.outputs["installer"]
		plugin.Logger(ctx).DebugContext(ctx, "Prepared", "artifact", artifact.Filename, "version", artifact.Version, "cached", p.item.Cached, "elapsed", time.Since(p.started).Round(time.Millisecond))
	case len(p.item.BlockedBy) > 0:
		plugin.Logger(ctx).DebugContext(ctx, "Resource blocked", "blocked_by", p.item.BlockedBy)
	default:
		plugin.Logger(ctx).DebugContext(ctx, "Preparation failed", "error", p.failure)
	}
	if p.failure != nil || e.pending[key] == 0 {
		return e.complete(ctx, &e.report.Resources[p.result.report])
	}
	return nil
}
