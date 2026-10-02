package cas

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/woodleighschool/stemma/plugin"
)

// Run owns one maintenance lifecycle around a complete command, including all
// its internal phases. The outer lease prevents collection between those phases.
func Run(ctx context.Context, dir string, policy Policy, offline bool, run func(context.Context) error) error {
	s, err := Open(dir)
	if err != nil {
		return err
	}
	return s.run(ctx, policy, offline, run)
}

func (s *Store) run(ctx context.Context, policy Policy, offline bool, run func(context.Context) error) error {
	failedRecency := &atomic.Bool{}
	ctx = context.WithValue(ctx, recencyKey{}, failedRecency)
	var reclaimed int64
	var maintenanceErr error
	var after Usage
	maintain := func(ctx context.Context) {
		result, err := s.collect(ctx, policy, PruneOptions{}, false, !offline && !failedRecency.Load())
		reclaimed += result.Reclaimed
		maintenanceErr = errors.Join(maintenanceErr, err)
		if result.Before != (Usage{}) {
			after = result.After
		}
	}
	defer func() {
		logger := plugin.Logger(ctx)
		if maintenanceErr != nil {
			logger.WarnContext(ctx, "Cache maintenance incomplete", "error", maintenanceErr)
		}
		over := !offline && policy.MaxSize > 0 && after.Retained > policy.MaxSize
		if reclaimed > 0 || over {
			message := fmt.Sprintf("Cache: reclaimed %s; %s retained", humanize.IBytes(uint64(max(0, reclaimed))), humanize.IBytes(uint64(max(0, after.Retained))))
			if over {
				message += fmt.Sprintf(" (above %s target; recent content protected)", humanize.IBytes(uint64(max(0, policy.MaxSize))))
			}
			logger.InfoContext(ctx, message, "cache_maintenance", true)
		}
	}()
	maintain(ctx)
	release, err := s.Lease(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Never upgrade our shared lease. Cancellation still permits bounded cleanup.
		maintenanceErr = errors.Join(maintenanceErr, release())
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		maintain(cleanup)
	}()
	return run(ctx)
}
