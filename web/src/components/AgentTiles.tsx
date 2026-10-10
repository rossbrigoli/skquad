"use client";

// S-211: agent tiles for the squad Overview screen. Replaces the old
// entity-row list: each agent is a tile with a robot icon on the left,
// then Name, Role and status. Running agents get a pulsing blue halo
// (2s loop, disabled under prefers-reduced-motion); failed/error agents
// get a static red outline.
//
// S-242: this is THE reusable agent tile — the dashboard's Squads
// section and the squad Overview page both render AgentTile/AgentTilesGrid
// so the two surfaces can never drift. Tiles are bigger and taller
// (~4:3, see globals.css .agent-tile) and now carry:
//   * the AI model the agent uses (muted, single-line truncated), and
//   * the task the agent is working on — current_task preferred over
//     last_task — as `REF · title…` on one nowrap/ellipsis line.
// The cost line (S-230) stays but callers no longer prefix "last 30 days".

import Link from "next/link";
import { IconRobot } from "./icons";
import { StatusChip } from "./StatusChip";
import { agentStatus, type StatusKey } from "../lib/status";
import type { Agent, AgentTaskBrief } from "../lib/api";

// agentTileClass maps a status to the tile modifier class. Kept exported
// so the class contract is unit-testable without rendering.
// S-242: optional `variant` appends a `agent-tile--<variant>` hook so
// embedding pages (e.g. the dashboard) can style tiles contextually.
export function agentTileClass(status: StatusKey, variant?: string): string {
  const parts = ["agent-tile"];
  if (variant) parts.push(`agent-tile--${variant}`);
  if (status === "running") parts.push("agent-tile--running");
  if (status === "error") parts.push("agent-tile--failed");
  return parts.join(" ");
}

// agentTaskLine picks the task to display (running/current beats last) and
// formats it as `REF · title`. The ref falls back to a short id prefix
// when the backend hasn't assigned a human ref. Returns null when the
// agent has no task at all — the tile then omits the line entirely.
export function agentTaskLine(agent: Pick<Agent, "current_task" | "last_task">): string | null {
  const task: AgentTaskBrief | null | undefined = agent.current_task ?? agent.last_task;
  if (!task?.id) return null;
  // S6606: explicit ternary — an empty ref must fall back to the id
  // prefix just like a missing one, so plain `??` would change behavior.
  const ref = task.ref ? task.ref : task.id.slice(0, 8);
  const title = (task.title ?? "").trim();
  if (!title) return ref;
  return `${ref} · ${title}`;
}

export function AgentTile({
  agent,
  href,
  costLabel,
  variant,
}: {
  readonly agent: Agent;
  readonly href: string;
  // S-230: optional cost line (e.g. the agent's rolling cost on the
  // dashboard's squad tiles). S-242: plain amount only — no "last 30
  // days" wording. Omitted on the squad Overview screen.
  readonly costLabel?: string;
  // S-242: optional styling hook (see agentTileClass).
  readonly variant?: string;
}) {
  const status = agentStatus(agent);
  const taskLine = agentTaskLine(agent);
  return (
    <Link href={href} className={agentTileClass(status, variant)} aria-label={`${agent.name} — ${status}`}>
      <span className="agent-tile-head">
        <span className="agent-tile-icon" aria-hidden="true">
          <IconRobot size={28} />
        </span>
        <span className="agent-tile-status">
          <StatusChip status={status} />
        </span>
      </span>
      <span className="agent-tile-body">
        <span className="agent-tile-name">{agent.name}</span>
        <span className="agent-tile-role">{agent.role ? agent.role : "no role set"}</span>
        {agent.model ? (
          <span className="agent-tile-model" title={agent.model}>
            {agent.model}
          </span>
        ) : null}
        {taskLine ? (
          <span className="agent-tile-task mono" title={taskLine}>
            {taskLine}
          </span>
        ) : null}
        {costLabel ? <span className="agent-tile-cost mono">{costLabel}</span> : null}
      </span>
    </Link>
  );
}

export function AgentTilesGrid({ agents }: { readonly agents: readonly Agent[] }) {
  return (
    <div className="agent-tile-grid">
      {agents.map((agent) => (
        <AgentTile key={agent.id} agent={agent} href={`/squads/${agent.squad_id}/agents/${agent.id}`} />
      ))}
    </div>
  );
}
