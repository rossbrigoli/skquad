-- 0052: TG-11 (S-257) — artifact executor (sysadmin mutation lane, docs/tool-gateway.md §6.7)
--
-- artifact_applies: one row per ssh_apply request. The approval binding is
-- (playbook, git_rev): git_rev is a full 40-char SHA that the executor
-- verified reachable from the resource's registered default branch
-- (merged = approved). Status 'refused' records why a request never ran
-- (artifact_not_merged, lint_failed, ...) with nothing applied.
--
-- The UNIQUE index implements apply idempotency: a duplicate request for
-- the same (resource, playbook, git_rev, host_group, check_only) returns
-- the existing record rather than re-applying.
--
-- drift_reports: periodic `ansible-playbook --check` results per
-- (resource, host_group, playbook, rev); drifted_hosts lists hosts whose
-- check-mode run reported changes, i.e. off the approved state.

CREATE TABLE artifact_applies (
    id              TEXT PRIMARY KEY,
    resource_id     UUID NOT NULL REFERENCES registry_resources(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL,
    playbook        TEXT NOT NULL,
    git_rev         TEXT NOT NULL CHECK (char_length(git_rev) = 40 AND git_rev ~ '^[0-9a-f]+$'),
    host_group      TEXT NOT NULL,
    check_only      BOOLEAN NOT NULL DEFAULT FALSE,
    force           BOOLEAN NOT NULL DEFAULT FALSE,
    status          TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','succeeded','failed','refused')),
    refusal_reason  TEXT,
    per_host        JSONB,
    lint_findings   JSONB,
    recording_id    TEXT,
    revert_of       TEXT,
    emergency       BOOLEAN NOT NULL DEFAULT FALSE,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ
);

CREATE UNIQUE INDEX artifact_applies_idem_idx
    ON artifact_applies (resource_id, playbook, git_rev, host_group, check_only);

CREATE INDEX artifact_applies_resource_time_idx
    ON artifact_applies (resource_id, requested_at DESC);

CREATE INDEX artifact_applies_agent_time_idx
    ON artifact_applies (agent_id, requested_at DESC);

CREATE TABLE drift_reports (
    id              TEXT PRIMARY KEY,
    resource_id     UUID NOT NULL REFERENCES registry_resources(id) ON DELETE CASCADE,
    host_group      TEXT NOT NULL,
    playbook        TEXT NOT NULL,
    git_rev         TEXT NOT NULL,
    checked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    drifted_hosts   JSONB NOT NULL DEFAULT '[]'::jsonb,
    in_sync         BOOLEAN NOT NULL
);

CREATE INDEX drift_reports_resource_group_time_idx
    ON drift_reports (resource_id, host_group, checked_at DESC);
