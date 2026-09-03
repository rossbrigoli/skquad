package httpapi

import (
	"context"
	"log/slog"
	"time"
)

// RunExecutionReaper periodically expires task executions whose lease has
// lapsed and re-queues their tasks so dead workers do not leave work stuck
// in-progress forever. It is safe to run on multiple replicas: the store
// update is conditional and idempotent, so concurrent reapers converge
// without a leader election.
//
// An execution is declared dead once its lease has expired for longer than
// grace, i.e. cutoff = now - grace. With the default 2-minute lease and
// 2-minute grace, a worker must miss heartbeats for ~4 minutes before its
// task is re-queued.
func RunExecutionReaper(ctx context.Context, store Store, interval, grace time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if grace < 0 {
		grace = 0
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reaped, err := store.ReapExpiredTaskExecutions(ctx, time.Now().Add(-grace))
			if err != nil {
				slog.Warn("reap expired task executions", "error", err)
				continue
			}
			if reaped > 0 {
				slog.Info("reaped expired task executions", "count", reaped, "grace", grace)
			}
		}
	}
}
