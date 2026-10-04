-- 0040: S-203 WP4 — budget events in the notification bell.
--
-- (1) notifications.type gains 'budget_warning' and 'budget_stopped' so
-- the 80%/90% warnings and the 100% stop also surface in the bell
-- (the inbox rows shipped with WP3; the bell was the gap).
ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_type_check;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_type_check
    CHECK (type IN ('task_failed','task_stuck','agent_died','task_blocked','budget_warning','budget_stopped'));

-- (2) squad_id becomes nullable: budget notifications are user-level
-- (a user's budget spans all their squads), unlike the task-scoped
-- S-193 alerts. Existing rows keep their squad references.
ALTER TABLE notifications ALTER COLUMN squad_id DROP NOT NULL;
