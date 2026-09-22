"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { ActivityFeed } from "../../../../../components/ActivityFeed";
import { AuthGate } from "../../../../../components/AuthGate";
import { AppShell } from "../../../../../components/AppShell";
import { EmptyState } from "../../../../../components/EmptyState";
import { MetricTile } from "../../../../../components/MetricTile";
import { SquadRail } from "../../../../../components/SquadRail";
import { StatusChip } from "../../../../../components/StatusChip";
import { useApi } from "../../../../../lib/useApi";
import { formatCost, formatRelativeTime, formatTokens, leaseState } from "../../../../../lib/format";
import { agentStatus } from "../../../../../lib/status";
import type {
  Agent,
  AgentPermission,
  AuditEntry,
  BoardPayload,
  MeteringSummary,
  Squad,
} from "../../../../../lib/api";

export default function AgentProfilePage() {
  const params = useParams<{ id: string; agentId: string }>();
  const squadId = String(params?.id || "");
  const agentId = String(params?.agentId || "");

  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 30000);
  const squads = useApi<Squad[]>("/squads");
  const board = useApi<BoardPayload>(`/squads/${squadId}/board`, 15000);
  const metering = useApi<MeteringSummary>(`/agents/${agentId}/metering`, 30000);
  const perms = useApi<AgentPermission[]>(`/agents/${agentId}/permissions`, 60000);
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=50`, 30000);

  const agent = (agents.data || []).find((a) => a.id === agentId);
  const squad = (squads.data || []).find((s) => s.id === squadId);
  const tasks = (board.data?.tasks || []).filter((t) => t.assignee_agent_id === agentId);
  const live = tasks.filter((t) => leaseState(t) === "running");
  const stalled = tasks.filter((t) => leaseState(t) === "stalled");
  const agentActivity = (audit.data || []).filter((e) => e.resource_id === agentId);

  if (!agent && !agents.loading) {
    return (
      <AuthGate>
        <AppShell secondary={<SquadRail squadId={squadId} squadName={squad?.name || "Squad"} />}>
          <EmptyState title="Agent not found" hint="It may have been removed from this squad." />
        </AppShell>
      </AuthGate>
    );
  }

  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName={squad?.name || "…"} />}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "var(--space-3)" }}>
          <h1 className="page-title">{agent?.name || "Agent"}</h1>
          {agent ? <StatusChip status={agentStatus(agent)} /> : null}
        </div>
        <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)" }}>
          {agent?.role || "no role set"} · <span className="mono">{agentId.slice(0, 12)}</span>
        </p>

        <div className="metric-grid" style={{ marginTop: "var(--space-4)" }}>
          <MetricTile
            label="Current lease"
            value={live.length > 0 ? "1 task" : "none"}
            sub={live.length > 0 ? live[0].title : stalled.length > 0 ? `${stalled.length} stalled task(s)` : "idle"}
            attention={stalled.length > 0}
          />
          <MetricTile
            label="Lifetime spend"
            value={metering.loading ? "…" : formatCost(metering.data)}
            sub={metering.error ? "owner and platform admins only" : formatTokens(metering.data)}
          />
          <MetricTile label="Assigned tasks" value={tasks.length} sub={`${live.length} running now`} />
          <MetricTile
            label="Granted resources"
            value={perms.data?.length ?? "—"}
            sub={(perms.data || []).length === 0 ? "no resource grants" : "see below"}
          />
        </div>

        {live.length > 0 ? (
          <section style={{ marginTop: "var(--space-5)" }}>
            <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Working on</h2>
            <div className="entity-list">
              {live.map((task) => (
                <Link key={task.id} href={`/squads/${squadId}/tasks/${task.id}`} className="entity-row">
                  <div className="entity-main">
                    <span className="entity-title">{task.title}</span>
                    <span className="entity-meta">lease expires {formatRelativeTime(task.lease_expires_at)}</span>
                  </div>
                  <div className="entity-side">
                    <StatusChip status="running" />
                  </div>
                </Link>
              ))}
            </div>
          </section>
        ) : null}

        {stalled.length > 0 ? (
          <section style={{ marginTop: "var(--space-5)" }}>
            <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Stalled work</h2>
            <div className="entity-list">
              {stalled.map((task) => (
                <Link key={task.id} href={`/squads/${squadId}/tasks/${task.id}`} className="entity-row">
                  <div className="entity-main">
                    <span className="entity-title">{task.title}</span>
                    <span className="entity-meta">lease expired {formatRelativeTime(task.lease_expires_at)}</span>
                  </div>
                  <div className="entity-side">
                    <StatusChip status="stalled" />
                  </div>
                </Link>
              ))}
            </div>
          </section>
        ) : null}

        <section style={{ marginTop: "var(--space-5)" }}>
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Granted resources</h2>
          {(perms.data || []).length === 0 ? (
            <EmptyState
              title="No resource grants"
              hint="This agent cannot reach any registered workspaces, APIs or knowledge bases yet."
            />
          ) : (
            <div className="entity-list">
              {(perms.data || []).map((grant) => (
                <div key={grant.id} className="entity-row">
                  <div className="entity-main">
                    <span className="entity-title">{grant.resource_type}</span>
                    <span className="entity-meta mono">{grant.resource_id.slice(0, 12)}</span>
                  </div>
                  <div className="entity-side">
                    <span className="entity-meta">{formatRelativeTime(grant.created_at)}</span>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Recent activity</h2>
          <ActivityFeed
            squadId={squadId}
            entries={agentActivity}
            emptyTitle="No recorded activity for this agent"
            emptyHint="Assignments, status changes and identity events show up here."
          />
        </section>
      </AppShell>
    </AuthGate>
  );
}
