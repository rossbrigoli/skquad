-- S-156: K8s resource naming conventions.
-- Agents carry their deterministic Deployment name (skquad-<owner>-agent-<agent>),
-- computed once at creation because names are immutable afterwards.
-- Existing agents keep deployment_name = '' and the operator falls back to
-- the CR-derived name, so no backfill churn is required.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS deployment_name TEXT NOT NULL DEFAULT '';
