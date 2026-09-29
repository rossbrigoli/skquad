-- 0024: Consult timeout/SLA (S-173) + dead-letter operations (S-174).
--
-- timeout_at: deadline by which an agent-sent consult must have received a
-- correlated reply, otherwise the asker gets a synthetic consult_timeout
-- notification. Set only on agent consults (chat/user messages never carry
-- it). timeout_notified_at is the sweeper's idempotency marker: a consult
-- times out at most once, so duplicate synthetic events are impossible even
-- across concurrent replicas (each row is claimed with SKIP LOCKED).
ALTER TABLE messages ADD COLUMN IF NOT EXISTS timeout_at timestamptz;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS timeout_notified_at timestamptz;

CREATE INDEX IF NOT EXISTS idx_messages_consult_timeout_sweep
    ON messages (timeout_at)
    WHERE timeout_at IS NOT NULL AND timeout_notified_at IS NULL;

-- S-174: admin dead-letter screen lists dead messages filtered by squad and
-- time; keep the scan cheap and bounded to the dead partition.
CREATE INDEX IF NOT EXISTS idx_messages_dead_squad_created
    ON messages (squad_id, created_at)
    WHERE status = 'dead';
