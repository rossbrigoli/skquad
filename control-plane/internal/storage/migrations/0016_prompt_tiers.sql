-- 0016: Prompt tiers + append-only revision history (S-PROMPT WP2, ADR-0011).
--
-- Layer 2 (organization): single-row instance settings table, seeded here
-- so the org tier always has a row to update (CHECK id = 1 keeps it
-- single-row, matching the plan's instance_settings pattern).
--
-- Layer 3 (squad): new squads.prompt column. `mission` stays as the short
-- listing summary; `prompt` is the full layer-3 text (Ross decision #4).
--
-- Revision history: prompt_revisions is APPEND-ONLY and retained FOREVER
-- (Ross decision 2026-09-27: storage is cheap, audit history is never
-- pruned). Every save of an editable tier appends one row IN THE SAME
-- TRANSACTION as the entity update, so a committed prompt change always
-- carries its revision and a failed revision write rolls the change back.
--
-- Idempotency: IF NOT EXISTS / ON CONFLICT, matching the 0013/0014/0015 style.

ALTER TABLE squads ADD COLUMN IF NOT EXISTS prompt text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS instance_settings (
    id          SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    org_name    TEXT NOT NULL DEFAULT '',
    org_prompt  TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  TEXT NOT NULL DEFAULT ''
);

INSERT INTO instance_settings (id) VALUES (1)
    ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS prompt_revisions (
    id         BIGSERIAL PRIMARY KEY,
    scope      TEXT NOT NULL CHECK (scope IN ('organization', 'squad', 'agent')),
    scope_id   TEXT NOT NULL DEFAULT '',            -- '' for the organization tier
    content    TEXT NOT NULL,
    tokens     INTEGER NOT NULL DEFAULT 0,          -- composer token estimate at save time
    sha256     TEXT NOT NULL,                      -- hex sha256 of content
    saved_by   TEXT NOT NULL DEFAULT '',
    saved_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_prompt_revisions_lookup
    ON prompt_revisions (scope, scope_id, saved_at DESC);
