// Attention-first merge (docs/UI-REDESIGN-PROPOSAL.md): the inbox is not a
// message list, it is a prioritised queue of everything that needs a human.
// Pure functions — no fetching here, so the ranking is unit-testable.

import { leaseState } from "./format";
import type { Agent, InboxMessage, Task } from "./api";

export type AttentionReason =
  | "stalled"
  | "agent_error"
  | "blocked"
  | "action_required"
  | "stale_review"
  | "completed";

export type AttentionItem = {
  id: string;
  reason: AttentionReason;
  title: string;
  href: string;
  meta: string;
  createdAt?: string;
};

// Lower number = more urgent. Stalled work means an agent died mid-task;
// completed notifications are noise until the urgent stuff is cleared.
export const REASON_PRIORITY: Record<AttentionReason, number> = {
  stalled: 0,
  agent_error: 1,
  blocked: 2,
  action_required: 3,
  stale_review: 4,
  completed: 5,
};

export const DEFAULT_STALE_REVIEW_MS = 24 * 60 * 60 * 1000;

export type AttentionInput = {
  // squadId -> live tasks per squad (board payloads already unwrapped).
  tasksBySquad: Record<string, Task[]>;
  agents: Agent[];
  inbox: InboxMessage[];
  now: number;
  staleReviewAfterMs?: number;
  agentName?: (id?: string) => string | undefined;
};

function isAgentError(agent: Agent): boolean {
  const status = (agent.status ?? "").toLowerCase();
  return status === "error" || status === "failed";
}

function parseAge(value?: string): number | null {
  if (!value) {
    return null;
  }
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? null : parsed;
}

// S3776: per-source collectors keep buildAttention's complexity down.
function taskAttentionItems(
  squadId: string,
  task: Task,
  nameOf: (id?: string) => string,
  now: number,
  staleAfter: number,
): AttentionItem[] {
  const out: AttentionItem[] = [];
  const taskHref = `/squads/${squadId}/tasks/${task.id}`;
  if (leaseState(task) === "stalled") {
    out.push({
      id: `stalled:${task.id}`,
      reason: "stalled",
      title: task.title,
      href: taskHref,
      meta: `${nameOf(task.assignee_agent_id)} lost its lease`,
      createdAt: task.updated_at,
    });
  }
  if (task.status === "blocked") {
    out.push({
      id: `blocked:${task.id}`,
      reason: "blocked",
      title: task.title,
      href: taskHref,
      meta: `blocked · ${nameOf(task.assignee_agent_id)}`,
      createdAt: task.updated_at,
    });
  }
  if (task.status === "in-review") {
    const age = parseAge(task.updated_at);
    if (age !== null && now - age >= staleAfter) {
      const days = Math.floor((now - age) / 86_400_000);
      out.push({
        id: `stale_review:${task.id}`,
        reason: "stale_review",
        title: task.title,
        href: taskHref,
        meta: `waiting ${days >= 1 ? `${days}d` : "over a day"} for review`,
        createdAt: task.updated_at,
      });
    }
  }
  return out;
}

function agentAttentionItems(agent: Agent): AttentionItem[] {
  if (!isAgentError(agent)) {
    return [];
  }
  return [
    {
      id: `agent_error:${agent.id}`,
      reason: "agent_error",
      title: agent.name,
      href: `/squads/${agent.squad_id}/agents/${agent.id}`,
      meta: `agent reported ${agent.status?.toLowerCase() || "error"}`,
      createdAt: agent.updated_at,
    },
  ];
}

function inboxMessageHref(message: InboxMessage): string {
  return message.task_id
    ? `/squads/${message.squad_id}/tasks/${message.task_id}`
    : `/squads/${message.squad_id}`;
}

function inboxAttentionItems(message: InboxMessage): AttentionItem[] {
  if (message.read_at) {
    return [];
  }
  if (message.kind === "action_required") {
    return [
      {
        id: `action:${message.id}`,
        reason: "action_required",
        title: message.message,
        href: inboxMessageHref(message),
        meta: "agent needs your input",
        createdAt: message.created_at,
      },
    ];
  }
  return [
    {
      id: `done:${message.id}`,
      reason: "completed",
      title: message.message,
      href: inboxMessageHref(message),
      meta: "completed",
      createdAt: message.created_at,
    },
  ];
}

export function buildAttention(input: AttentionInput): AttentionItem[] {
  const staleAfter = input.staleReviewAfterMs ?? DEFAULT_STALE_REVIEW_MS;
  const nameOf = (id?: string): string =>
    input.agentName?.(id) ?? (id ? id.slice(0, 8) : "unassigned");
  const items: AttentionItem[] = [];

  for (const [squadId, tasks] of Object.entries(input.tasksBySquad)) {
    for (const task of tasks) {
      items.push(...taskAttentionItems(squadId, task, nameOf, input.now, staleAfter));
    }
  }

  for (const agent of input.agents) {
    items.push(...agentAttentionItems(agent));
  }

  for (const message of input.inbox) {
    items.push(...inboxAttentionItems(message));
  }

  items.sort((a, b) => {
    const byPriority = REASON_PRIORITY[a.reason] - REASON_PRIORITY[b.reason];
    if (byPriority !== 0) {
      return byPriority;
    }
    const aTime = parseAge(a.createdAt) ?? 0;
    const bTime = parseAge(b.createdAt) ?? 0;
    return bTime - aTime;
  });

  return items;
}
