// Skquad UI v2 — unified status vocabulary.
// One key per state, rendered identically everywhere (chip, row, tile, log).
// Design basis: docs/UI-REDESIGN-PROPOSAL.md §3.3

import type { Agent, Task } from "./api";
import { leaseState } from "./format";

export type StatusKey =
  | "running"
  | "stalled"
  | "blocked"
  | "in-review"
  | "idle"
  | "error"
  | "over-budget"
  | "paused"
  | "todo"
  | "done";

export const statusLabels: Record<StatusKey, string> = {
  running: "Running",
  stalled: "Stalled",
  blocked: "Blocked",
  "in-review": "In review",
  idle: "Idle",
  error: "Error",
  "over-budget": "Over budget",
  paused: "Paused",
  todo: "To do",
  done: "Done",
};

// Attention ordering: most urgent first. Used by the inbox and any sort-by-need.
export const statusAttention: Record<StatusKey, number> = {
  error: 0,
  "over-budget": 1,
  stalled: 2,
  blocked: 3,
  "in-review": 4,
  running: 5,
  paused: 6,
  todo: 7,
  idle: 8,
  done: 9,
};

export function taskStatus(task: Task): StatusKey {
  if (task.status === "blocked") return "blocked";
  if (task.status === "in-review") return "in-review";
  if (task.status === "done") return "done";
  const lease = leaseState(task);
  if (lease === "running") return "running";
  if (lease === "stalled") return "stalled";
  return "todo";
}

export function agentStatus(agent: Agent): StatusKey {
  const status = (agent.status ?? "").toLowerCase();
  if (status === "error" || status === "failed") return "error";
  if (status === "paused") return "paused";
  if (status === "busy") return "running";
  return "idle";
}
