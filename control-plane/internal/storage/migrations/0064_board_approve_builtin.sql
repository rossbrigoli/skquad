-- 0064: S-294 (ADR-0016 D5/D8) board.approve — leader approval of
-- OTHER agents' in-review tasks.
--
--   board.approve -> POST /api/v1/agents/me/tasks/{id}/approve
--
-- Server-side gates (default deny): the calling agent must hold a leader
-- grant on the squad (board_policy.leader_grants, can_prioritize and/or
-- can_assign_others) AND the squad's board_policy.leader_approval_allowed
-- must be enabled (platform default false — approval strictly human).
-- The grants themselves live in the existing squads.board_policy JSONB
-- document (migration 0061) — no schema change here, only the builtin
-- tool name CHECK widening (same pattern as 0063) and the enabled seed
-- (mirrors domain.BuiltinToolDefaultEnabled).
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent','read','write','edit','apply_patch','process','cron','sessions_list','sessions_history','session_status','browser','board.create_task','board.assign','board.claim','board.complete','board.block','board.reopen','board.request_changes','board.approve'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES
        ('board.approve', TRUE, '{}')
    ON CONFLICT (name) DO NOTHING;
