package ops

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Scheduler runs the ops jobs in the background on a fixed interval. It is started
// in the composition root for RUN_MODE control|all (single replica, decision #4):
// exactly one process owns the destructive reaper so two replicas never race a
// bundle+remove. Stop() drains the loop within the parent context.
type Scheduler struct {
	ops      *Ops
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// NewScheduler wraps an Ops with the periodic runner.
func NewScheduler(o *Ops, logger *slog.Logger) *Scheduler {
	return &Scheduler{ops: o, interval: o.cfg.Interval, logger: logger}
}

// Start launches the background loop. It runs the one-shot startup consistency
// check immediately (clearing stale locks before the first writes), then ticks the
// periodic reaper + gc every interval until Stop or the parent context is done.
// Start is idempotent; a second call is a no-op.
func (s *Scheduler) Start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started = true

	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})

	go s.loop(ctx)
}

// loop is the scheduler goroutine.
func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)

	// Startup consistency check: clear stale flocks + log orphans/dangling rows
	// once, before the periodic jobs and before heavy write traffic builds up.
	if _, err := s.ops.Consistency(ctx); err != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "ops: startup consistency check failed", "err", err)
	}

	// Run one periodic pass immediately so a freshly-booted single-replica instance
	// does not wait a full interval to reclaim already-past-grace sites.
	s.runOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce executes the periodic jobs (reaper, gc) once, isolating each so one
// failure does not skip the other.
func (s *Scheduler) runOnce(ctx context.Context) {
	if s.ops.cfg.EnableReaper {
		if _, err := s.ops.Reap(ctx); err != nil && s.logger != nil {
			s.logger.WarnContext(ctx, "ops: reaper pass failed", "err", err)
		}
	}
	if s.ops.cfg.EnableGC {
		if err := s.ops.GC(ctx); err != nil && s.logger != nil {
			s.logger.WarnContext(ctx, "ops: gc pass failed", "err", err)
		}
	}
}

// Stop signals the loop to exit and waits for it to drain. Safe to call once; a
// call before Start (or twice) is a no-op.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel, done, started := s.cancel, s.done, s.started
	s.mu.Unlock()
	if !started || cancel == nil {
		return
	}
	cancel()
	<-done
}
