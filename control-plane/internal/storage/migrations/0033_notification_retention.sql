-- 0033: S-198 notification retention + inbox-delete audit.
--
-- No new tables. The inbox-delete audit (S-198) reuses the existing
-- append-only audit_log with action='inbox.deleted', and the retention
-- sweep (read notifications older than the window) needs two indexes so
-- its DELETEs stay index-scans instead of full-table scans:
--
--   * partial index on notifications for the read-older-than-cutoff purge
--     (the read_at IS NOT NULL predicate is what protects unread alerts;
--     inbox messages live in another table and are never swept — S-193).
--   * action+timestamp for the audit purge, which is scoped to
--     action='inbox.deleted' so unrelated audit history is untouched.

CREATE INDEX IF NOT EXISTS idx_notifications_read_created
    ON notifications (created_at)
    WHERE read_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_audit_action_time
    ON audit_log (action, timestamp);
