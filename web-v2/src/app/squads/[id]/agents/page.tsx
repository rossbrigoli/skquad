"use client";

import { useParams } from "next/navigation";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { EmptyState } from "../../../../components/EmptyState";
import { EntityRow } from "../../../../components/EntityRow";
import { SquadRail } from "../../../../components/SquadRail";
import { StatusChip } from "../../../../components/StatusChip";
import { useApi } from "../../../../lib/useApi";
import { agentStatus } from "../../../../lib/status";
import type { Agent } from "../../../../lib/api";

export default function SquadAgentsPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id || "");
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 15000);
  const items = agents.data || [];

  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName="Squad" />}>
        <h1 className="page-title">Agents</h1>
        {agents.error ? <div className="notice error">{agents.error}</div> : null}
        {!agents.loading && items.length === 0 ? (
          <EmptyState title="No agents in this squad" hint="Add agents from the v1 UI until agent management lands in v2." />
        ) : (
          <div className="entity-list">
            {items.map((agent) => (
              <EntityRow
                key={agent.id}
                href={`/squads/${squadId}/agents`}
                title={agent.name}
                meta={`${agent.role || "no role"} · ${agent.default_model || "no default model"}`}
                side={<StatusChip status={agentStatus(agent)} />}
              />
            ))}
          </div>
        )}
        <p style={{ color: "var(--ink-faint)", fontSize: "var(--text-sm)", marginTop: "var(--space-4)" }}>
          Full agent profiles (leases, runs, spend, grants) land in UIv2-6.
        </p>
      </AppShell>
    </AuthGate>
  );
}
