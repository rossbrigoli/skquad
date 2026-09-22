"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { ActivityFeed } from "../../../components/ActivityFeed";
import { AuthGate } from "../../../components/AuthGate";
import { AppShell } from "../../../components/AppShell";
import { EmptyState } from "../../../components/EmptyState";
import { MetricTile } from "../../../components/MetricTile";
import { SquadRail } from "../../../components/SquadRail";
import { StatusChip } from "../../../components/StatusChip";
import { useApi } from "../../../lib/useApi";
import { formatCost, formatRelativeTime, leaseState } from "../../../lib/format";
import { agentStatus, taskStatus } from "../../../lib/status";
import type { Agent, BoardPayload, MeteringSummary, Squad, AuditEntry } from "../../../lib/api";

export default function SquadCockpitPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id || "");
  const squads = useApi<Squad[]>("/squads");
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 15000);
  const board = useApi<BoardPayload>(`/squads/${squadId}/board`, 15000);
  const metering = useApi<MeteringSummary>(`/squads/${squadId}/metering`, 30000);
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=15`, 30000);

  const squad = (squads.data || []).find((item) => item.id === squadId);
  const agentItems = agents.data || [];
  const tasks = board.data?.tasks || [];
  const running = tasks.filter((task) => leaseState(task) === "running");
  const stalled = tasks.filter((task) => leaseState(task) === "stalled");
  const blocked = tasks.filter((task) => task.status === "blocked");
  const done = tasks.filter((task) => task.status === "done");
  const busyAgents = agentItems.filter((agent) => (agent.status || "") === "busy");
  const errorAgents = agentItems.filter((agent) => {
    const s = (agent.status || "").toLowerCase();
    return s === "error" || s === "failed";
  });

  const agentName = (id?: string) =>
    agentItems.find((agent) => agent.id === id)?.name || (id ? id.slice(0, 8) : "unassigned");

  const auditName = (entry: AuditEntry) => {
    if (entry.actor_type === "agent") {
      return agentName(entry.actor_id);
    }
    return undefined;
  };

  const taskHref = (taskId: string) => `/squads/${squadId}/tasks/${taskId}`;

  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName={squad?.name || "…"} />}>
        <h1 className="page-title">{squad?.name || "Squad"}</h1>
        {squad?.mission ? (
          <p style={{ color: "var(--ink-muted)", marginTop: "calc(-1 * var(--space-3))", marginBottom: "var(--space-5)" }}>
            {squad.mission}
          </p>
        ) : null}

        <div className="metric-grid">
          <MetricTile
            label="Agents"
            value={agentItems.length}
            sub={`${busyAgents.length} busy · ${agentItems.length - busyAgents.length - errorAgents.length} idle${errorAgents.length > 0 ? ` · ${errorAgents.length} error` : ""}`}
            attention={errorAgents.length > 0}
          />
          <MetricTile
            label="Work in flight"
            value={running.length}
            sub={`${tasks.length - done.length} open · ${blocked.length} blocked${stalled.length > 0 ? ` · ${stalled.length} stalled` : ""}`}
            attention={stalled.length > 0}
          />
          <MetricTile
            label="Squad spend"
            value={metering.loading ? "…" : formatCost(metering.data)}
            sub={metering.error ? "owner and platform admins only" : "lifetime"}
          />
          <MetricTile label="Tasks done" value={done.length} sub={`of ${tasks.length} total`} />
        </div>

        <section>
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Live runs</h2>
          {running.length === 0 ? (
            <EmptyState
              title={stalled.length > 0 ? "Nothing running — stalled work needs attention" : "No agents working right now"}
              hint={stalled.length > 0 ? `${stalled.length} task(s) lost their worker heartbeat.` : "Assign a task and the squad picks it up."}
            />
          ) : (
            <div className="entity-list">
              {running.map((task) => (
                <Link key={task.id} href={taskHref(task.id)} className="entity-row">
                  <div className="entity-main">
                    <span className="entity-title">{task.title}</span>
                    <span className="entity-meta">
                      {agentName(task.assignee_agent_id)} · lease expires {formatRelativeTime(task.lease_expires_at)}
                    </span>
                  </div>
                  <div className="entity-side">
                    <StatusChip status={taskStatus(task)} />
                  </div>
                </Link>
              ))}
            </div>
          )}
        </section>

        {stalled.length > 0 ? (
          <section style={{ marginTop: "var(--space-5)" }}>
            <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Stalled</h2>
            <div className="entity-list">
              {stalled.map((task) => (
                <Link key={task.id} href={taskHref(task.id)} className="entity-row">
                  <div className="entity-main">
                    <span className="entity-title">{task.title}</span>
                    <span className="entity-meta">
                      last held by {agentName(task.assignee_agent_id)} · lease expired {formatRelativeTime(task.lease_expires_at)}
                    </span>
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
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Recent activity</h2>
          <ActivityFeed
            squadId={squadId}
            entries={audit.data || []}
            emptyTitle="No recorded activity yet"
            emptyHint="Actions on tasks, agents and grants show up here as they happen."
            nameFor={auditName}
          />
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Agents</h2>
          <div className="entity-list">
            {agentItems.map((agent) => (
              <Link key={agent.id} href={`/squads/${squadId}/agents`} className="entity-row">
                <div className="entity-main">
                  <span className="entity-title">{agent.name}</span>
                  <span className="entity-meta">{agent.role || "no role set"}</span>
                </div>
                <div className="entity-side">
                  <StatusChip status={agentStatus(agent)} />
                </div>
              </Link>
            ))}
          </div>
        </section>
      </AppShell>
    </AuthGate>
  );
}
