-- 0025: per-agent thinking level (S-178).
--
-- Adds the reasoning-effort knob surfaced in the agent screen composer.
-- Allowed values: '' (unset → provider default), 'low', 'medium', 'high'.
-- The value flows CP → Agent CR (spec.thinkingLevel) → runtime env
-- (SKQUAD_THINKING_LEVEL) → litellm reasoning_effort on LLM requests.
ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS thinking_level TEXT NOT NULL DEFAULT '';

ALTER TABLE agents
    DROP CONSTRAINT IF EXISTS agents_thinking_level_check;

ALTER TABLE agents
    ADD CONSTRAINT agents_thinking_level_check
    CHECK (thinking_level IN ('', 'low', 'medium', 'high'));
