-- S-162: chat reset boundary.
--
-- agents.chat_reset_at marks the instant the user reset the agent chat
-- thread. All messages created at or before this instant stop being
-- included in the chat history (UI list) and in the runtime's
-- contextual prompt. The transcript is archived into agent_memory
-- (provenance='chat_reset') before the boundary is set, so the agent
-- can still recall it semantically.
--
-- NULL means "never reset" — every message counts, as before.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS chat_reset_at TIMESTAMPTZ;
