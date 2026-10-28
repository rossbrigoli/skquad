-- S-303: persist the fingerprint of the provider API key last pushed to
-- each model's LiteLLM gateway deployment.
--
-- Rationale: a provider API-key rotation leaves the gateway deployment
-- serving the OLD key. The pre-save Test button validates the provider's
-- CURRENT key directly (bypasses the gateway) so it passes, while the
-- agent runtime goes through the gateway and hits a 401 on the stale
-- key. The gateway never returns the plaintext key (/model/info redacts
-- it) and silently drops unknown model_info fields, so the fingerprint
-- must live in skquad's own registry. Reconcile compares this stored
-- fingerprint against the provider's current key and re-keys the
-- deployment when they diverge.
--
-- Empty string = never provisioned with a fingerprinted key (legacy row);
-- the first reconcile re-keys and stamps it.
ALTER TABLE ai_models
    ADD COLUMN gateway_key_fingerprint TEXT NOT NULL DEFAULT '';
