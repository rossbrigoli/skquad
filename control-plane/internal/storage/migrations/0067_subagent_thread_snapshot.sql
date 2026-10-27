-- S-301: task-subagent transparency rework.
--
-- The S-299 design streamed every nested turn into the task thread as a
-- message row. Those rows surfaced in the main agent chat feed (all
-- messages addressed to the agent) and in the chat task card, flooding
-- both with "[subagent] turn N - tool -> ok" noise.
--
-- New model: the runner publishes ONE replaceable snapshot of its
-- captured thread per turn onto the execution row. The chat task card
-- gets a Details button that opens the right-hand SubagentThreadPanel
-- (same affordance as the regular spawn_subagent chip), polled from
-- GET /tasks/{id}/subagent-thread. No message rows are created.
ALTER TABLE task_executions
    ADD COLUMN IF NOT EXISTS subagent_thread JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Purge the historical noise rows S-299 wrote: per-turn "[subagent] ..."
-- lines and the stream-capped notice. They live in the messages table
-- addressed to the assignee, so they polluted both the chat feed and the
-- task thread. Lifecycle events ("Delegating this task...", completion
-- summaries) are untouched.
DELETE FROM messages
    WHERE payload ? 'task_id'
      AND (
          payload->>'message' LIKE '[subagent]%'
          OR payload->>'message' LIKE '...progress stream capped%'
      );
