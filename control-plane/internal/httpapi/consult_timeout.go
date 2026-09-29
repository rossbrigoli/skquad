package httpapi

// S-173: consult timeout/SLA sweeper.
//
// A consult is a question with a deadline: if the target agent never
// answers, the asker should learn that instead of waiting forever. The
// send path stamps agent consults with timeout_at (default 15 minutes,
// per-send overridable via consult_timeout_seconds); this loop periodically
// asks the store to post synthetic consult_timeout replies for expired,
// unanswered consults.
//
// Like the execution reaper, it is safe on multiple replicas: the store
// claims due rows with SKIP LOCKED and stamps timeout_notified_at, so
// every consult times out exactly once platform-wide.
//
// Exclusions handled in the store's sweep query:
//   - delegate/handoff never carry a deadline (task loop, not a consult)
//   - consults that already have a correlated reply from the target
//   - dead/expired consults (already terminal — visible as dead letters)
//   - deleted askers (stamped, nothing posted)
//
// Scale-to-zero: the default deadline (15 min) comfortably exceeds cold
// start latency, so a waking agent is not timed out mid-wake. Operators
// can raise it via SKQUAD_CONSULT_TIMEOUT_SECONDS.

import (
	"context"
	"log/slog"
	"time"
)

func RunConsultTimeoutSweeper(ctx context.Context, store Store, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			posted, err := store.SweepConsultTimeouts(ctx, time.Now().UTC())
			if err != nil {
				slog.Warn("consult timeout sweep", "error", err)
				continue
			}
			if posted > 0 {
				slog.Info("consult timeouts notified", "count", posted)
			}
		}
	}
}
