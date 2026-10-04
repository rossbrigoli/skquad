-- 0041: S-203 WP4 fix — widen inbox_messages.kind for budget events.
--
-- WP3 shipped the budget_warning/budget_stopped inbox writes in code,
-- but the CHECK constraint from 0029 was never widened, so every
-- Postgres insert failed with SQLSTATE 23514 (MemoryStore tests don't
-- exercise CHECK constraints, which is how it slipped). The bell emit
-- added in 0040 surfaced it: the inbox write now precedes the bell
-- emit, and both are gated by the same once-per-month marker.
ALTER TABLE inbox_messages
    DROP CONSTRAINT IF EXISTS inbox_messages_kind_check;
ALTER TABLE inbox_messages
    ADD CONSTRAINT inbox_messages_kind_check
    CHECK (kind IN ('task_completed','action_required','agent_message','budget_warning','budget_stopped'));
