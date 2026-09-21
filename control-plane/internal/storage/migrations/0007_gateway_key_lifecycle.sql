-- 0007: LLM gateway virtual-key lifecycle on agent identities.
-- gateway_key_token stores the LiteLLM key token (sha256 hash of the virtual
-- key, not the plaintext key) so the control plane can update or revoke the
-- key when the agent's LLM permissions change.

ALTER TABLE agent_identities ADD COLUMN IF NOT EXISTS gateway_key_token text NOT NULL DEFAULT '';
ALTER TABLE agent_identities ADD COLUMN IF NOT EXISTS gateway_key_status text NOT NULL DEFAULT 'none';
