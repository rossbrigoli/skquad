-- 0010: wake-path latency events (S-87).
-- One row per cold/warm task delivery: task assignment (wake trigger, the
-- upsert_agent outbox event) through control-plane queue, CR write, container
-- start, and the runtime's claim. Segments are derived at claim time; e2e_ms
-- is the SLO number (target p95 < 20s, see docs/observability-metering.md).
-- Deduped per (agent, container start) so a pod records at most one wake.
CREATE TABLE IF NOT EXISTS wake_latency (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id           uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    squad_id           uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    task_id            uuid REFERENCES tasks(id) ON DELETE SET NULL,
    wake_requested_at  timestamptz NOT NULL,
    cr_applied_at      timestamptz NOT NULL,
    container_started_at timestamptz NOT NULL,
    claimed_at         timestamptz NOT NULL,
    queue_ms           double precision NOT NULL,
    scaleup_ms         double precision NOT NULL,
    claim_delay_ms     double precision NOT NULL,
    e2e_ms             double precision NOT NULL,
    cold_start         boolean NOT NULL DEFAULT true,
    UNIQUE (agent_id, container_started_at)
);
CREATE INDEX IF NOT EXISTS idx_wake_latency_squad ON wake_latency(squad_id, claimed_at);
