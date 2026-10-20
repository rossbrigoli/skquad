-- S-278 (re-scope of S-275): task dependencies.
--
-- A task may declare "depends on" one or more other tasks. A task is
-- BLOCKED for agent pickup while any of its depends-on tasks is not
-- 'done'. The guard is enforced server-side at claim/start time (see
-- storage.ClaimNextTask / httpapi start guard) — not merely hidden in
-- the UI.
--
-- Invariants:
--   * No self-dependency (CHECK).
--   * Both endpoints must belong to the same squad (enforced in the
--     storage layer, since squad_id lives on tasks, not derivable in a
--     table CHECK).
--   * No cycles: adding task→dep is rejected when dep already depends
--     (transitively) on task. Enforced in the storage layer with a
--     recursive CTE at insert time (a pure CHECK constraint cannot see
--     the rest of the graph).
--   * Cascade delete: dependencies vanish with either endpoint task.
CREATE TABLE IF NOT EXISTS task_dependencies (
    task_id            uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    depends_on_task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_by_type    text NOT NULL CHECK (created_by_type IN ('user','agent')),
    created_by_id      uuid NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, depends_on_task_id),
    CONSTRAINT task_dependencies_no_self CHECK (task_id <> depends_on_task_id)
);

-- Reverse lookup: "what blocks / what is blocked by this task".
CREATE INDEX IF NOT EXISTS idx_task_dependencies_depends_on
    ON task_dependencies (depends_on_task_id);
