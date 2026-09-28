-- S-155: provider API keys move into control-plane-managed Kubernetes
-- Secrets. api_key_ref becomes a k8s://<namespace>/<secret> reference;
-- api_key_mask stores the display tail (last 5 chars) so the UI never
-- needs the Secret to render. The startup migration in the control-plane
-- wraps any pre-existing literal keys and rewrites both columns.
ALTER TABLE providers ADD COLUMN IF NOT EXISTS api_key_mask text NOT NULL DEFAULT '';
