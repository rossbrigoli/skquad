-- 0049: TG-8 slice B — grant-request state machine + tier routing
-- (docs/tg8-grant-approvals-spec.md §B).
--
-- The TG-2 placeholder grant_requests table was schema-only: no Go code ever
-- read or wrote it (verified by grep across control-plane, tool-gateway,
-- shared). It is renamed to grant_requests_tg2_legacy to preserve any rows,
-- and the spec-shaped table is created fresh under the canonical name.
ALTER TABLE IF EXISTS grant_requests RENAME TO grant_requests_tg2_legacy;

CREATE TABLE IF NOT EXISTS grant_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID NOT NULL REFERENCES registry_resources(id) ON DELETE CASCADE,
    -- agent_id is intentionally NOT a FK: the workflow record must survive
    -- agent deletion (audit trail), and the service layer validates agent
    -- existence at create/materialize time.
    agent_id UUID NULL,
    requester_user_id UUID NOT NULL REFERENCES users(id),
    tier TEXT NOT NULL CHECK (tier IN ('low','medium','high')),
    state TEXT NOT NULL DEFAULT 'pending_owner'
        CHECK (state IN ('pending_owner','pending_admin','approved','denied')),
    -- requested_scope carries the constraint payload the eventual grant
    -- materializes with (⊆ resource policy_ceiling, validated service-side).
    requested_scope JSONB NOT NULL DEFAULT '{}',
    findings_json JSONB NOT NULL DEFAULT '[]',
    approved_by_owner_at TIMESTAMPTZ NULL,
    approved_by_admin_at TIMESTAMPTZ NULL,
    denied_reason TEXT NOT NULL DEFAULT '',
    expiry TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_grant_requests_state ON grant_requests(state);
CREATE INDEX IF NOT EXISTS idx_grant_requests_resource ON grant_requests(resource_id);
CREATE INDEX IF NOT EXISTS idx_grant_requests_requester ON grant_requests(requester_user_id);

-- Inbox kinds for grant-request OUTCOMES. pending_owner / pending_admin
-- reuse the generic 'action_required' kind (the task statement is the
-- message); approved/denied need their own kinds so the recipient can
-- tell the outcome apart from a to-do. Widening mirrors the precedent in
-- 0029/0041 (agent_message, budget kinds).
ALTER TABLE inbox_messages
    DROP CONSTRAINT IF EXISTS inbox_messages_kind_check;
ALTER TABLE inbox_messages
    ADD CONSTRAINT inbox_messages_kind_check
    CHECK (kind IN ('task_completed','action_required','agent_message','budget_warning','budget_stopped','grant_approved','grant_denied'));
