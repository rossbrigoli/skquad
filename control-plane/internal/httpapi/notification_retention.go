package httpapi

// S-198: notification retention sweep.
//
// Read notifications used to accumulate forever. This sweep purges READ
// notifications older than the retention window (default 90 days).
// Unread notifications are never touched, and inbox messages are NEVER
// auto-removed (S-193 semantics: the inbox only loses messages through an
// explicit user delete). The same sweep also purges the audit rows this
// feature writes — action='inbox.deleted' (see deleteInboxMessage) —
// older than the window; every other audit_log action is left alone.
//
// Pattern follows RunExecutionReaper / RunStuckTaskScanner: a goroutine
// started from main with the deterministic core exported for tests. Safe
// to run on every replica: the purge is idempotent, so concurrent sweeps
// converge (worst case, one of them purges zero).

import (
	"context"
	"log/slog"
	"time"
)

// auditInboxDeleted is the audit_log action recorded when a user deletes
// an inbox message (S-198: inbox deletes were previously a silent 204).
// The retention sweep purges only audit rows carrying this action.
const auditInboxDeleted = "inbox.deleted"

// RunNotificationRetention sweeps every interval, purging read
// notifications and inbox-delete audit rows older than retention.
// Failures are logged and never fatal — the loop must survive a bad sweep.
func RunNotificationRetention(ctx context.Context, store Store, interval, retention time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	if retention <= 0 {
		retention = 90 * 24 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			notifs, audit, err := PurgeNotificationsOnce(ctx, store, time.Now().UTC().Add(-retention))
			if err != nil {
				slog.Warn("notification retention sweep failed", "error", err)
				continue
			}
			if notifs > 0 || audit > 0 {
				slog.Info("notification retention purged",
					"notifications", notifs, "audit_rows", audit, "retention", retention)
			}
		}
	}
}

// PurgeNotificationsOnce runs a single sweep with an explicit cutoff and
// returns (notifications purged, audit rows purged). Exported for
// deterministic tests; the loop above is just the scheduler.
func PurgeNotificationsOnce(ctx context.Context, store Store, cutoff time.Time) (int, int, error) {
	notifs, err := store.DeleteReadNotificationsBefore(ctx, cutoff)
	if err != nil {
		return 0, 0, err
	}
	audit, err := store.DeleteAuditByActionBefore(ctx, auditInboxDeleted, cutoff)
	if err != nil {
		return notifs, 0, err
	}
	return notifs, audit, nil
}
