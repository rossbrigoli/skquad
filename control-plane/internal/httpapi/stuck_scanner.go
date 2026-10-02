package httpapi

// S-197: stuck-task scanner.
//
// The execution reaper (S-193) catches workers that died mid-task. This
// scanner catches the quieter failure mode: a task parked in-progress with
// nobody actually making progress — no task-thread activity and no
// execution heartbeat for N hours (default 24h). The squad owner gets one
// task_stuck bell alert per task per N hours.
//
// Like the reaper, it is safe to run on every replica: the sweep is a read
// and the dedupe is a query against the notifications table, so concurrent
// scanners converge on at most one alert per task per window (a benign
// race two replicas filing simultaneously is bounded by the same window
// and acceptable; the reaper makes no stronger guarantee for its own
// notifications).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// RunStuckTaskScanner sweeps every interval and files task_stuck
// notifications for in-progress tasks whose latest thread activity AND
// heartbeat progress are older than threshold. threshold doubles as the
// dedupe window: a task that already received a task_stuck alert within
// the last `threshold` is skipped, so a permanently-stuck task re-alerts
// at most once per window. Recipient is the squad owner; the S-199 mute
// preference is honored. All failures are logged and never fatal — the
// loop must survive a bad sweep.
func RunStuckTaskScanner(ctx context.Context, store Store, interval, threshold time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if threshold <= 0 {
		threshold = 24 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			filed, err := ScanStuckTasksOnce(ctx, store, threshold)
			if err != nil {
				slog.Warn("stuck task scan failed", "error", err)
				continue
			}
			if filed > 0 {
				slog.Info("filed task_stuck notifications", "count", filed, "threshold", threshold)
			}
		}
	}
}

// ScanStuckTasksOnce runs a single sweep and returns the number of
// notifications filed. Exported for deterministic tests; the loop above
// is just the scheduler.
func ScanStuckTasksOnce(ctx context.Context, store Store, threshold time.Duration) (int, error) {
	if threshold <= 0 {
		threshold = 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-threshold)
	tasks, err := store.ListStaleInProgressTasks(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	filed := 0
	for _, task := range tasks {
		// Dedupe: at most one task_stuck per task per threshold window,
		// judged by the prior notification's created_at — no new column.
		recent, err := store.HasRecentNotificationForTask(ctx, task.ID, domain.NotificationTaskStuck, cutoff)
		if err != nil {
			slog.Warn("stuck dedupe lookup failed", "error", err, "task_id", task.ID)
			continue
		}
		if recent {
			continue
		}
		squad, err := store.GetSquad(ctx, task.SquadID)
		if err != nil || squad.OwnerID == "" {
			continue
		}
		// S-199: honor the recipient's mute list (fail-open on lookup error).
		if notificationMuted(ctx, store, squad.OwnerID, domain.NotificationTaskStuck) {
			slog.Info("notification skipped: type muted by user preference",
				"type", string(domain.NotificationTaskStuck), "user", squad.OwnerID)
			continue
		}
		msg := fmt.Sprintf("Task %s has been in-progress for over %s with no thread activity or heartbeat progress.",
			formatTaskRef(task), formatStuckThreshold(threshold))
		_, err = store.CreateNotification(ctx, &domain.Notification{
			UserID:   squad.OwnerID,
			SquadID:  task.SquadID,
			TaskID:   task.ID,
			AgentID:  task.AssigneeAgentID,
			Type:     domain.NotificationTaskStuck,
			Severity: domain.NotificationWarning,
			Message:  msg,
		})
		if err != nil {
			// ErrNotFound ⇒ the owner/squad row is gone: nothing addressable.
			if !errors.Is(err, storage.ErrNotFound) {
				slog.Warn("file task_stuck notification", "error", err, "task_id", task.ID)
			}
			continue
		}
		filed++
	}
	return filed, nil
}

// formatStuckThreshold renders the threshold for the alert message in
// whole hours or minutes ("24h", "90m") instead of Go's raw duration.
func formatStuckThreshold(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return d.String()
	}
}
