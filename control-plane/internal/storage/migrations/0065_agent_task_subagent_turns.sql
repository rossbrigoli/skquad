-- S-298: per-agent task-subagent turn budget.
-- NULL = no per-agent override; the platform default (128) applies.
-- When set (>0) it flows to the Agent CR (spec.taskSubagentMaxTurns)
-- and on to the agent pod as SKQUAD_TASK_SUBAGENT_MAX_TURNS.
ALTER TABLE agents
    ADD COLUMN task_subagent_max_turns INTEGER
    CHECK (task_subagent_max_turns IS NULL OR task_subagent_max_turns BETWEEN 1 AND 512);
