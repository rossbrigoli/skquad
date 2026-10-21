-- 0057: S-281 built-in CRON / scheduled tasks (cron).
--
-- scheduled_jobs: durable records of future agent turns. The scheduler
-- goroutine in the API server claims due rows (enabled + next_run_at
-- due, FOR UPDATE SKIP LOCKED so replicas never double-fire) and fires
-- them through the canonical agent-run path: a task materialized on the
-- agent's board plus the busy status mirror — the same wake mechanism
-- delegate/handoff messages use. One-shot ('at') jobs flip enabled=FALSE
-- after firing; jobs that fail 5 consecutive fires auto-disable with the
-- reason recorded in disabled_reason.
--
-- schedule_spec is JSONB pinned per kind (validated in Go, domain/
-- scheduled_jobs.go):
--   at:    {"at": "2026-10-21T19:30:00+09:30"}
--   every: {"interval_seconds": 3600}
--   cron:  {"expression": "0 9 * * 1-5", "timezone": "Australia/Adelaide"}
CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id        uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    squad_id        uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    title           TEXT NOT NULL DEFAULT '',
    message         TEXT NOT NULL,
    schedule_kind   TEXT NOT NULL CHECK (schedule_kind IN ('at','every','cron')),
    schedule_spec   JSONB NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    next_run_at     TIMESTAMPTZ NOT NULL,
    last_run_at     TIMESTAMPTZ,
    last_run_status TEXT NOT NULL DEFAULT '',
    fail_count      INTEGER NOT NULL DEFAULT 0,
    disabled_reason TEXT NOT NULL DEFAULT '',
    created_by_type TEXT NOT NULL DEFAULT 'agent',
    created_by_id   uuid NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The scheduler's claim query is an (enabled, next_run_at) range scan;
-- partial index keeps it small as history accumulates disabled rows.
CREATE INDEX IF NOT EXISTS scheduled_jobs_due_idx
    ON scheduled_jobs (enabled, next_run_at);

-- Agent-scoped listing (`cron list`) rides the agent FK index.
CREATE INDEX IF NOT EXISTS scheduled_jobs_agent_idx
    ON scheduled_jobs (agent_id);

-- Widen the builtin_tools_config name CHECK to admit the CRON tool
-- (same pattern as 0055/0056) and seed it ENABLED with the default
-- per-agent active-job cap.
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent','read','write','edit','apply_patch','process','cron'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES
        ('cron', TRUE, '{"maxActiveJobs": 10}')
    ON CONFLICT (name) DO NOTHING;
