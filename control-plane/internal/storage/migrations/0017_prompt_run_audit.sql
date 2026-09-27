-- 0017: Server-side run-audit prompt hash (S-PROMPT WP5, ADR-0011 D5).
--
-- Table choice: task_executions is the authoritative run-record table —
-- one row per fenced runtime attempt, created at claim (createTaskExecutionTx)
-- and terminalized at complete/block/expiry. The composed prompt sha is a
-- property of the RUN, not of the task: the same task can execute many
-- times, each run potentially under a different composed prompt (tiers
-- edited between runs). tasks therefore stays sha-free; the sha rides the
-- execution row, matching the plan §1.2 ("whichever table backs the
-- run record is authoritative").
--
-- Value semantics:
--   * 64-char lowercase hex — sha256 of the composed effective prompt
--     served by GET /api/v1/agents/me/prompt (WP3 fetch-at-wake).
--   * 'env_legacy'         — the runtime ran on the pre-composition
--     fallback path (SKQUAD_PROMPT_FETCH_ENABLED off / 404).
--   * '' (empty)           — a runtime older than WP5 that did not
--     report a sha; treated as "unknown", never as "verified".
--
-- Idempotency: IF NOT EXISTS, matching the 0013–0016 style.
ALTER TABLE task_executions ADD COLUMN IF NOT EXISTS prompt_sha TEXT NOT NULL DEFAULT '';

-- Forensics query: "which runs used this exact composed prompt"
-- (blast-radius of a bad tier edit, incident response).
CREATE INDEX IF NOT EXISTS idx_task_executions_prompt_sha
    ON task_executions(prompt_sha);
