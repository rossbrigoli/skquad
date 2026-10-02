-- 0031: AI model vision-input capability (S-200).
-- Adds supports_vision to ai_models, parallel to supports_tools. The
-- agent runtime embeds attached images as base64 content parts only when
-- the bound model is vision-capable; the flag is also provisioned into
-- the LiteLLM gateway deployment model_info for defense-in-depth gating.
ALTER TABLE ai_models ADD COLUMN IF NOT EXISTS supports_vision boolean NOT NULL DEFAULT false;
