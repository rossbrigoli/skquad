-- 0018: Built-in platform tools config (BT-2, ADR-0012).
--
-- One row per built-in tool, seeded disabled. The control plane is the
-- single source of truth for exec / web_fetch / web_search enablement and
-- policy; runtimes fetch their view via GET /api/v1/agents/me/tools and
-- never see provider credentials.
--
-- Idempotency: IF NOT EXISTS + ON CONFLICT DO NOTHING, matching the
-- 0013-0017 style.
CREATE TABLE IF NOT EXISTS builtin_tools_config (
    name        TEXT PRIMARY KEY CHECK (name IN ('exec','web_fetch','web_search')),
    enabled     BOOLEAN NOT NULL DEFAULT FALSE,
    policy      JSONB   NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  TEXT    NOT NULL DEFAULT ''
);

-- Seed: all three tools present and disabled (ADR-0012 §1).
INSERT INTO builtin_tools_config (name) VALUES ('exec'), ('web_fetch'), ('web_search')
    ON CONFLICT (name) DO NOTHING;
