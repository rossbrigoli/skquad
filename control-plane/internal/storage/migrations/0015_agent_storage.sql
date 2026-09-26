-- 0015: Per-agent durable workspace storage settings (S-138).
--
-- Adds the agents-table columns behind "storage size on agent create/edit":
--   * storage_enabled: whether the agent gets a durable workspace PVC
--     (flows to Agent CR spec.storage.enabled). Default false keeps every
--     existing agent exactly as before.
--   * storage_size: Kubernetes quantity string requested for the PVC
--     (spec.storage.size). Default '2Gi' mirrors the CRD default from
--     S-135 so DB, CRD and UI agree on the platform default.
--
-- storageClass is intentionally NOT a per-agent column: it stays
-- platform-admin only (SKQUAD_STORAGE_CLASS at the control plane), per
-- the S-135 portability rule.
--
-- Idempotency: ADD COLUMN IF NOT EXISTS, matching the 0013/0014 style.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS storage_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS storage_size    text    NOT NULL DEFAULT '2Gi';
