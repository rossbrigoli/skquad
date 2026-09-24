"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { ActivityFeed } from "../../../components/ActivityFeed";
import { Collapsible } from "../../../components/Collapsible";
import { ConfirmDialog } from "../../../components/ConfirmDialog";
import { Modal, ModalForm } from "../../../components/Modal";
import { AuthGate } from "../../../components/AuthGate";
import { AppShell } from "../../../components/AppShell";
import { EmptyState } from "../../../components/EmptyState";
import { MetricTile } from "../../../components/MetricTile";
import { SquadRail } from "../../../components/SquadRail";
import { StatusChip } from "../../../components/StatusChip";
import { useApi } from "../../../lib/useApi";
import { useAuth } from "../../../lib/auth";
import { apiDelete, apiPatch } from "../../../lib/api";
import { formatCost, formatRelativeTime, leaseState } from "../../../lib/format";
import { agentStatus, taskStatus } from "../../../lib/status";
import type { Agent, BoardPayload, MeteringSummary, Squad, AuditEntry } from "../../../lib/api";

export default function SquadCockpitPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id || "");
  const router = useRouter();
  const { token } = useAuth();
  const squads = useApi<Squad[]>("/squads");
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
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
        <div className="section-head">
          <h1 className="page-title" style={{ margin: 0 }}>
            {squad?.name || "Squad"}
          </h1>
          <div style={{ display: "flex", gap: "var(--space-2)" }}>
            <button type="button" className="btn btn-sm" onClick={() => setEditing(true)} disabled={!squad}>
              Edit
            </button>
            <button type="button" className="btn btn-sm btn-danger" onClick={() => setDeleting(true)} disabled={!squad}>
              Delete
            </button>
          </div>
        </div>
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
          <div className="section-head">
            <h2>Agents</h2>
            <Link href={`/squads/${squadId}/agents`} className="btn btn-sm">
              Manage agents
            </Link>
          </div>
          <div className="entity-list">
            {agentItems.map((agent) => (
              <Link key={agent.id} href={`/squads/${squadId}/agents/${agent.id}`} className="entity-row">
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

        <Collapsible id="squad-recent-activity" title="Recent activity">
          <ActivityFeed
            squadId={squadId}
            entries={audit.data || []}
            emptyTitle="No recorded activity yet"
            emptyHint="Actions on tasks, agents and grants show up here as they happen."
            nameFor={auditName}
          />
        </Collapsible>

        {editing && squad ? (
          <SquadEditModal
            squad={squad}
            onClose={() => setEditing(false)}
            onSaved={() => {
              setEditing(false);
              squads.refresh();
            }}
            token={token}
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

function SquadEditModal({
  squad,
  onClose,
  onSaved,
  token,
}: {
  squad: Squad;
  onClose: () => void;
  onSaved: () => void;
  token: string;
}) {
  const [name, setName] = useState(squad.name || "");
  const [mission, setMission] = useState(squad.mission || "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title="Edit squad" onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        onCancel={onClose}
        submitLabel="Save changes"
        submitDisabled={name.trim() === ""}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            await apiPatch<Squad>(`/squads/${squad.id}`, token, { name: name.trim(), mission: mission.trim() });
            onSaved();
          } catch (err) {
            setError(err instanceof Error ? err.message : "update failed");
            setBusy(false);
          }
        }}
      >
        <label className="field">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
        </label>
        <label className="field">
          <span>Mission</span>
          <textarea value={mission} onChange={(e) => setMission(e.target.value)} />
        </label>
      </ModalForm>
    </Modal>
  );
}
