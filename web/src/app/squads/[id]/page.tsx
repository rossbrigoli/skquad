"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { ActivityFeed } from "../../../components/ActivityFeed";
import { AgentFormModal } from "../../../components/AgentForm";
import { AgentTilesGrid } from "../../../components/AgentTiles";
import { Collapsible } from "../../../components/Collapsible";
import { ConfirmDialog } from "../../../components/ConfirmDialog";
import { AuthGate } from "../../../components/AuthGate";
import { AppShell } from "../../../components/AppShell";
import { MetricTile } from "../../../components/MetricTile";
import { StatusChip } from "../../../components/StatusChip";
import { useApi } from "../../../lib/useApi";
import { useAuth } from "../../../lib/auth";
import { apiDelete, apiPost } from "../../../lib/api";
import { formatCost, formatRelativeTime, leaseState } from "../../../lib/format";
import { workInFlight } from "../../../lib/squadStats";
import type { AIModel } from "../../../lib/aimodels";
import type { Agent, BoardPayload, MeteringSummary, Squad, AuditEntry } from "../../../lib/api";

export default function SquadCockpitPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id ?? "");
  const router = useRouter();
  const { token } = useAuth();
  const squads = useApi<Squad[]>("/squads");
  const [deleting, setDeleting] = useState(false);
  // S-211: the new-agent dialog now lives on Overview (the Agents tab is gone).
  const [creating, setCreating] = useState(false);
  const myModels = useApi<AIModel[]>("/models/me", 60000);
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 15000);
  const board = useApi<BoardPayload>(`/squads/${squadId}/board`, 15000);
  const metering = useApi<MeteringSummary>(`/squads/${squadId}/metering`, 30000);
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=15`, 30000);

  const squad = (squads.data || []).find((item) => item.id === squadId);
  const agentItems = agents.data || [];
  const tasks = board.data?.tasks || [];
  const running = tasks.filter((task) => leaseState(task) === "running");
  const stalled = tasks.filter((task) => leaseState(task) === "stalled");
  const wip = workInFlight(tasks);
  const done = tasks.filter((task) => task.status === "done");
  const busyAgents = agentItems.filter((agent) => (agent.status ?? "") === "busy");
  const errorAgents = agentItems.filter((agent) => {
    const s = (agent.status ?? "").toLowerCase();
    return s === "error" || s === "failed";
  });

  const agentName = (id?: string) =>
    agentItems.find((agent) => agent.id === id)?.name ?? (id ? id.slice(0, 8) : "unassigned");

  const auditName = (entry: AuditEntry) => {
    if (entry.actor_type === "agent") {
      return agentName(entry.actor_id);
    }
    return undefined;
  };

  const taskHref = (taskId: string) => `/squads/${squadId}/tasks/${taskId}`;

  return (
    <AuthGate>
      <AppShell>
        <div className="section-head">
          <h1 className="page-title" style={{ margin: 0 }}>
            {squad?.name ?? "Squad"}
          </h1>
          <div style={{ display: "flex", gap: "var(--space-2)" }}>
            {/* S-179: the Edit dialog is gone — the mission now lives in
                the inline Configuration section below. Delete stays. */}
            <button type="button" className="btn btn-sm btn-danger" onClick={() => setDeleting(true)} disabled={!squad}>
              Delete
            </button>
          </div>
        </div>

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
            sub={`${wip.open} open · ${wip.blocked} blocked${stalled.length > 0 ? ` · ${stalled.length} stalled` : ""}`}
            attention={stalled.length > 0}
          />
          <MetricTile
            label="Squad spend"
            value={metering.loading ? "…" : formatCost(metering.data)}
            sub={metering.error ? "owner and platform admins only" : "lifetime"}
          />
          <MetricTile label="Tasks done" value={done.length} sub={`of ${tasks.length} total`} />
        </div>

        {/* S-170: the "Live runs" section was removed. It only ever
            tracked board-task leases — chat turns never create task
            executions, so it read "empty" while agents were visibly
            talking, and the "Work in flight" tile above already carries
            the running count. The Stalled section below stays because it
            is actionable (expired leases need a human). */}

        {/* S-211: the inline Configuration section was removed. The
            mission now lives on the Squad Context tab (above the context
            editor) and is saved together with it in one action. */}

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
          <div className="section-head">
            <h2>Agents</h2>
            {/* S-211: "Manage agents" is gone — the Agents tab was folded
                into Overview, so the create-agent affordance moved here. */}
            <button type="button" className="btn btn-sm btn-primary" onClick={() => setCreating(true)}>
              + New agent
            </button>
          </div>
          <AgentTilesGrid agents={agentItems} />
        </section>

        <Collapsible id="squad-recent-activity" title="Recent activity">
          <ActivityFeed
            squadId={squadId}
            entries={audit.data || []}
            emptyTitle="No recorded activity yet"
            emptyHint="Actions on tasks, agents and grants show up here as they happen."
            nameFor={auditName}
          />
        </Collapsible>

        {creating ? (
          <AgentFormModal
            title="New agent"
            submitLabel="Create agent"
            models={myModels.data ?? []}
            modelsLoading={myModels.loading}
            onClose={() => setCreating(false)}
            onSubmit={async (values) => {
              await apiPost<Agent>(`/squads/${squadId}/agents`, token, values);
              setCreating(false);
              agents.refresh();
            }}
          />
        ) : null}

        {deleting && squad ? (
          <ConfirmDialog
            title={`Delete squad “${squad.name}”?`}
            body="This removes the squad, its agents, board, tasks, grants and Kubernetes namespace. This cannot be undone."
            confirmText={squad.name}
            onConfirm={async () => {
              await apiDelete(`/squads/${squadId}`, token);
              router.push("/squads");
            }}
            onClose={() => setDeleting(false)}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}
