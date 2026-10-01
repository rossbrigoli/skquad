-- S-184: per-squad sequential task reference number.
-- Gives every task a short human-referenceable id (rendered "T-<n>" in the UI)
-- so long titles stay quotable. Numbering is per squad (a squad has exactly one
-- board, so squad-scoped == board-scoped) and immutable after creation.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS task_number integer;

-- Backfill existing rows: oldest task per squad gets 1.
WITH ranked AS (
    SELECT id, row_number() OVER (PARTITION BY squad_id ORDER BY created_at, id) AS rn
    FROM tasks
)
UPDATE tasks
SET task_number = ranked.rn
FROM ranked
WHERE tasks.id = ranked.id;

ALTER TABLE tasks ALTER COLUMN task_number SET NOT NULL;

-- One number per squad; also serves lookups by (squad_id, task_number).
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_squad_number ON tasks(squad_id, task_number);
