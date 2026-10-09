-- S-261: friendly per-agent workspace PVC names.
-- Agents carry their durable workspace PVC name (<owner>-<squad>-<agent>-workspace-<guid>),
-- computed once at creation because PVC names are immutable afterwards.
-- Existing agents keep workspace_pvc_name = '' and the operator falls back to
-- the legacy agent-<cr-name>-workspace derivation, so no backfill churn and no
-- renames of live volumes are required.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS workspace_pvc_name TEXT NOT NULL DEFAULT '';
