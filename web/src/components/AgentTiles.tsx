"use client";

// S-211: agent tiles for the squad Overview screen. Replaces the old
// entity-row list: each agent is a tile with a robot icon on the left,
// then Name, Role and status. Running agents get a pulsing blue halo
// (2s loop, disabled under prefers-reduced-motion); failed/error agents
// get a static red outline.

import Link from "next/link";
import { IconRobot } from "./icons";
import { StatusChip } from "./StatusChip";
import { agentStatus, type StatusKey } from "../lib/status";
import type { Agent } from "../lib/api";

// agentTileClass maps a status to the tile modifier class. Kept exported
// so the class contract is unit-testable without rendering.
export function agentTileClass(status: StatusKey): string {
  const parts = ["agent-tile"];
  if (status === "running") parts.push("agent-tile--running");
  if (status === "error") parts.push("agent-tile--failed");
  return parts.join(" ");
}

export function AgentTile({
  agent,
  href,
}: {
  readonly agent: Agent;
  readonly href: string;
}) {
  const status = agentStatus(agent);
  return (
    <Link href={href} className={agentTileClass(status)} aria-label={`${agent.name} — ${status}`}>
      <span className="agent-tile-icon" aria-hidden="true">
        <IconRobot size={28} />
      </span>
      <span className="agent-tile-body">
        <span className="agent-tile-name">{agent.name}</span>
        <span className="agent-tile-role">{agent.role ? agent.role : "no role set"}</span>
      </span>
      <span className="agent-tile-status">
        <StatusChip status={status} />
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
