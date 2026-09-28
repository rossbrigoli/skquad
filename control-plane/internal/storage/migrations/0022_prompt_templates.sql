-- S-158: admin-managed prompt templates.
-- Templates pre-populate the squad/agent system prompt at creation time;
-- they are copied into the draft, never referenced live.
CREATE TABLE IF NOT EXISTS prompt_templates (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL UNIQUE,
    description  text NOT NULL DEFAULT '',
    content      text NOT NULL,
    applies_to   text NOT NULL DEFAULT 'agent' CHECK (applies_to IN ('squad', 'agent', 'both')),
    created_by   uuid NOT NULL REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_prompt_templates_applies ON prompt_templates(applies_to);
