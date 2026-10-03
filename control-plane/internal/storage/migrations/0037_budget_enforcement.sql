-- S-203 WP3: Budget enforcement state + notification markers.
--
-- budget_blocks persists the per-user blocked state (monthly period so
-- the reset at calendar-month boundaries needs no cron). Two sources
-- (own budget vs platform-wide limit) are tracked independently so
-- clearing one does not clear the other.
--
-- budget_notify_markers records which 80/90/100 notifications already
-- fired for a user in a period. The primary key makes claiming a marker
-- an atomic INSERT ... ON CONFLICT DO NOTHING, which is the
-- exactly-once-per-threshold-per-month guard under concurrent turns.

CREATE TABLE IF NOT EXISTS budget_blocks (
    user_id             uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    period              text        NOT NULL,
    blocked_by_user     boolean     NOT NULL DEFAULT false,
    blocked_by_platform boolean     NOT NULL DEFAULT false,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS budget_notify_markers (
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    period     text        NOT NULL,
    marker     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, period, marker)
);

-- S-203 WP3: budget notifications are user-level, not squad-scoped.
-- squad_id becomes optional; task links in the UI already fall back to
-- "no link" when squad/task are absent.
ALTER TABLE inbox_messages ALTER COLUMN squad_id DROP NOT NULL;
