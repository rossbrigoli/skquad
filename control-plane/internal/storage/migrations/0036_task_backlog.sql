-- S-213: Backlog column.
-- Extend the tasks.status CHECK with 'backlog', the parking column for tasks
-- that are not yet ready to be started. Backlog is excluded from every
-- agent-facing pickup/listing path in the control plane; a human moving a
-- card out of Backlog is the instruction to start it.
ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_status_check,
    ADD CONSTRAINT tasks_status_check
        CHECK (status IN ('backlog','todo','in-progress','in-review','done','blocked'));
