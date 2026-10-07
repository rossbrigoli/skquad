-- 0050: TG-8 slice C — confirmation gates + standing grants
-- (docs/tg8-grant-approvals-spec.md §C).
--
-- pending_confirmations: one gated tool call awaiting the resource
-- owner's decision. Bound to the exact call via args_hash (sha256 of
-- canonical call args, computed by the gateway). approved_at drives the
-- 15-minute one-shot TTL checked at consume time; consumed_at makes the
-- single-use property durable (second consume → denied_replayed).
CREATE TABLE IF NOT EXISTS pending_confirmations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID NOT NULL REFERENCES registry_resources(id) ON DELETE CASCADE,
    -- agent_id intentionally has no FK: the decision record must survive
    -- agent deletion (slice B precedent, 0049).
    agent_id UUID NOT NULL,
    tool TEXT NOT NULL,
    args_hash TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','approved_once','approved_standing','denied','expired')),
    -- Link to the owner's action_required inbox message (best-effort:
    -- set after the message is created).
    inbox_message_id TEXT NULL,
    -- The resource owner's user id, resolved from the resource at
    -- request time — the authority for this confirmation.
    requested_by UUID NOT NULL REFERENCES users(id),
    denied_reason TEXT NOT NULL DEFAULT '',
    approved_at TIMESTAMPTZ NULL,
    consumed_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_pending_confirmations_state ON pending_confirmations(state);
CREATE INDEX IF NOT EXISTS idx_pending_confirmations_call ON pending_confirmations(resource_id, tool, args_hash);

-- standing_grants: persistent "approve this and future" decisions.
-- The gateway/CP checks these FIRST so repeated gated calls never page
-- the owner again until expiry. Revoke is soft (revoked_at set) — the
-- row survives as audit trail, and because the LIVE unique index only
-- covers revoked_at IS NULL rows, revocation removes the live authority
-- in one row update (invariant 2: immediately effective).
CREATE TABLE IF NOT EXISTS standing_grants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID NOT NULL REFERENCES registry_resources(id) ON DELETE CASCADE,
    -- Specific agent for v1; the '*' literal is reserved for a future
    -- all-agent scope and is NOT matched by v1 lookups.
    agent_id TEXT NOT NULL,
    tool TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_standing_grants_live
    ON standing_grants(resource_id, agent_id, tool) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_standing_grants_resource ON standing_grants(resource_id);

-- Inbox kind for confirmation OUTCOMES. The pending ask reuses the
-- generic 'action_required' kind (slice B precedent); the deny outcome
-- needs its own kind so the recipient can tell outcome from to-do.
ALTER TABLE inbox_messages
    DROP CONSTRAINT IF EXISTS inbox_messages_kind_check;
ALTER TABLE inbox_messages
    ADD CONSTRAINT inbox_messages_kind_check
    CHECK (kind IN ('task_completed','action_required','agent_message','budget_warning','budget_stopped','grant_approved','grant_denied','confirmation_denied'));
