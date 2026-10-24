-- 0063: S-292 (ADR-0016 D1) board.* builtins — agent-driven board work.
--
-- Seven verbs, each mapped 1:1 to a validated control-plane transition:
--   board.create_task      -> POST /api/v1/agents/me/tasks            (backlog default)
--   board.assign          -> PATCH /api/v1/agents/me/tasks/{id}/assignee
--   board.claim          -> POST /api/v1/agents/me/tasks/{id}/start  (fenced execution)
--   board.complete       -> POST /api/v1/agents/me/tasks/{id}/complete (review_needed)
--   board.block          -> POST /api/v1/agents/me/tasks/{id}/block
--   board.reopen         -> POST /api/v1/agents/me/tasks/{id}/reopen
--   board.request_changes-> POST /api/v1/agents/me/tasks/{id}/request-changes
--
-- No free-form status write exists: the server's transition guards
-- (column allowlists, leases, fencing, dependency guard S-278, budget
-- guard S-203, self-approval policy S-290, reopen cap S-290) are the
-- enforcement surface. Widen the builtin_tools_config name CHECK to
-- admit the seven tools (same pattern as 0055-0059) and seed them
-- ENABLED (mirrors domain.BuiltinToolDefaultEnabled).
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent','read','write','edit','apply_patch','process','cron','sessions_list','sessions_history','session_status','browser','board.create_task','board.assign','board.claim','board.complete','board.block','board.reopen','board.request_changes'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES
        ('board.create_task', TRUE, '{}'),
        ('board.assign', TRUE, '{}'),
        ('board.claim', TRUE, '{}'),
        ('board.complete', TRUE, '{}'),
        ('board.block', TRUE, '{}'),
        ('board.reopen', TRUE, '{}'),
        ('board.request_changes', TRUE, '{}')
    ON CONFLICT (name) DO NOTHING;
