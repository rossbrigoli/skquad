-- S-300: allow the derived "blocked" agent status.
--
-- An agent whose only remaining open work is a todo with unsatisfied
-- upstream dependencies is neither running nor idle: the work exists but
-- ClaimNextTask (S-278) cannot reach it. The control plane derives this
-- state; the runtime never self-reports it.
--
-- agents.status carried an inline CHECK from 0001_init.sql, so Postgres
-- auto-named it agents_status_check. Widening it is additive: existing
-- rows are untouched (idle/busy/error all still satisfy the new set).
ALTER TABLE agents
    DROP CONSTRAINT IF EXISTS agents_status_check;

ALTER TABLE agents
    ADD CONSTRAINT agents_status_check
    CHECK (status IN ('idle', 'busy', 'error', 'blocked'));
