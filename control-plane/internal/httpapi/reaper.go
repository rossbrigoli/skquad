package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
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
			if len(reaped) > 0 {
				slog.Info("reaped expired task executions", "count", len(reaped), "grace", grace)
			}
			// S-193: every dead attempt is an "agent died mid-task" alert
			// for the squad owner. Best-effort: a notification failure
			// must never stop the reaper loop.
			for _, r := range reaped {
				notifyReapedExecution(ctx, store, r)
			}
		}
	}
}

// notifyReapedExecution files one agent_died notification for a reaped
// attempt. All lookups are best-effort: if the task, agent, or squad owner
// is gone there is nothing addressable, so we skip silently.
func notifyReapedExecution(ctx context.Context, store Store, r domain.ReapedExecution) {
	task, err := store.GetTask(ctx, r.TaskID)
	if err != nil {
		return
	}
	agent, err := store.GetAgent(ctx, r.AgentID)
	if err != nil {
		return
	}
	squad, err := store.GetSquad(ctx, task.SquadID)
	if err != nil || squad.OwnerID == "" {
		return
	}
	// S-199: honor the recipient's mute list (fail-open on lookup error).
	if notificationMuted(ctx, store, squad.OwnerID, domain.NotificationAgentDied) {
		slog.Info("notification skipped: type muted by user preference",
			"type", string(domain.NotificationAgentDied), "user", squad.OwnerID)
		return
	}
	msg := fmt.Sprintf("Task %s failed: agent %s died mid-task (lease expired); the task was re-queued.",
		formatTaskRef(task), agent.Name)
	if _, err := store.CreateNotification(ctx, &domain.Notification{
		UserID:   squad.OwnerID,
		SquadID:  task.SquadID,
		TaskID:   task.ID,
		AgentID:  agent.ID,
		Type:     domain.NotificationAgentDied,
		Severity: domain.NotificationError,
		Message:  msg,
	}); err != nil && err != storage.ErrNotFound {
		slog.Warn("notify reaped execution", "error", err)
	}
}
