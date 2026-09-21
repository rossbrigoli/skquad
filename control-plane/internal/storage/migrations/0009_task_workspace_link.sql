-- 0009: git workspace linkage for tasks.
-- When a task runs against a granted git workspace, the runtime reports the
-- branch it worked on and the head commit it pushed, so the task carries an
-- audit trail back to the workspace commits. All optional: user-created tasks
-- and tasks without a granted workspace leave these empty.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS workspace_resource_id text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS workspace_branch      text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS workspace_commit_sha text NOT NULL DEFAULT '';
