"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { ActivityFeed } from "../../../../../components/ActivityFeed";
import { AgentFormModal } from "../../../../../components/AgentForm";
import { AuthGate } from "../../../../../components/AuthGate";
import { AppShell } from "../../../../../components/AppShell";
import { ConfirmDialog } from "../../../../../components/ConfirmDialog";
import { EmptyState } from "../../../../../components/EmptyState";
import { MetricTile } from "../../../../../components/MetricTile";
import { Modal, ModalForm } from "../../../../../components/Modal";
import { SquadRail } from "../../../../../components/SquadRail";
import { StatusChip } from "../../../../../components/StatusChip";
import { useApi } from "../../../../../lib/useApi";
import { useAuth } from "../../../../../lib/auth";
import {
  apiDelete,
  apiPatch,
  apiPost,
  apiPut,
  type Agent,
  type AgentPermission,
  type AuditEntry,
  type BoardPayload,
  type LLMProvider,
  type Message,
  type MeteringSummary,
  type RegistryResource,
  type ResourceType,
  type Squad,
} from "../../../../../lib/api";
import { formatCost, formatRelativeTime, formatTokens, leaseState } from "../../../../../lib/format";
import { agentStatus } from "../../../../../lib/status";

const GRANTABLE_TYPES: { type: ResourceType; path: string; label: string }[] = [
  { type: "skill", path: "skills", label: "Skills" },
  { type: "tool", path: "tools", label: "Tools" },
  { type: "api", path: "apis", label: "APIs" },
  { type: "knowledge_base", path: "knowledge-bases", label: "Knowledge bases" },
  { type: "project_workspace", path: "project-workspaces", label: "Project workspaces" },
];

export default function AgentProfilePage() {
  const params = useParams<{ id: string; agentId: string }>();
  const squadId = String(params?.id || "");
  const agentId = String(params?.agentId || "");
  const router = useRouter();
  const { token } = useAuth();

  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 30000);
  const squads = useApi<Squad[]>("/squads");
  const board = useApi<BoardPayload>(`/squads/${squadId}/board`, 15000);
  const metering = useApi<MeteringSummary>(`/agents/${agentId}/metering`, 30000);
  const perms = useApi<AgentPermission[]>(`/agents/${agentId}/permissions`, 60000);
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=50`, 30000);
  const chat = useApi<Message[]>(`/agents/${agentId}/chat`, 10000);
  const providers = useApi<LLMProvider[]>("/registry/llm-providers", 60000);

  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [granting, setGranting] = useState(false);
  const [identityBusy, setIdentityBusy] = useState(false);
  const [identityError, setIdentityError] = useState("");

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
        <div className="section-head">
          <div style={{ display: "flex", alignItems: "baseline", gap: "var(--space-3)" }}>
            <h1 className="page-title" style={{ margin: 0 }}>
              {agent?.name || "Agent"}
            </h1>
            {agent ? <StatusChip status={agentStatus(agent)} /> : null}
          </div>
          <div style={{ display: "flex", gap: "var(--space-2)" }}>
            <button type="button" className="btn btn-sm" onClick={() => setEditing(true)} disabled={!agent}>
              Edit
            </button>
            <button type="button" className="btn btn-sm btn-danger" onClick={() => setDeleting(true)} disabled={!agent}>
              Delete
            </button>
          </div>
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
          <div className="section-head">
            <h2>Talk to {agent?.name || "this agent"}</h2>
          </div>
          <ChatThread messages={chat.data || []} agentName={agent?.name || "agent"} onSent={() => chat.refresh()} agentId={agentId} token={token} />
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <div className="section-head">
            <h2>Granted resources</h2>
            <button type="button" className="btn btn-sm" onClick={() => setGranting(true)}>
              + Grant access
            </button>
          </div>
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
                    <button
                      type="button"
                      className="btn btn-sm btn-danger"
                      onClick={async () => {
                        const next = (perms.data || [])
                          .filter((p) => p.id !== grant.id)
                          .map((p) => ({ resource_type: p.resource_type, resource_id: p.resource_id }));
                        await apiPut(`/agents/${agentId}/permissions`, token, next);
                        perms.refresh();
                      }}
                    >
                      Revoke
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <div className="section-head">
            <h2>Runtime identity</h2>
            {agent?.identity_id ? (
              <button
                type="button"
                className="btn btn-sm"
                disabled={identityBusy}
                onClick={async () => {
                  setIdentityBusy(true);
                  setIdentityError("");
                  try {
                    await apiPost(`/agents/${agentId}/identity/rotate`, token, {});
                    agents.refresh();
                  } catch (err) {
                    setIdentityError(err instanceof Error ? err.message : "rotate failed");
                  } finally {
                    setIdentityBusy(false);
                  }
                }}
              >
                Rotate identity
              </button>
            ) : (
              <button
                type="button"
                className="btn btn-sm btn-primary"
                disabled={identityBusy}
                onClick={async () => {
                  setIdentityBusy(true);
                  setIdentityError("");
                  try {
                    await apiPost(`/agents/${agentId}/identity`, token, {});
                    agents.refresh();
                  } catch (err) {
                    setIdentityError(err instanceof Error ? err.message : "provision failed");
                  } finally {
                    setIdentityBusy(false);
                  }
                }}
              >
                Provision identity
              </button>
            )}
          </div>
          {identityError ? <div className="notice error">{identityError}</div> : null}
          <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)", margin: 0 }}>
            {agent?.identity_id
              ? "Identity provisioned — credentials live in the squad namespace as projected secrets. Rotating invalidates the previous credential."
              : "This agent has no runtime identity yet. It cannot connect to the control plane until an identity is provisioned. The agent needs at least one granted LLM provider model."}
          </p>
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

        {editing && agent ? (
          <AgentFormModal
            title={`Edit ${agent.name}`}
            submitLabel="Save changes"
            providers={providers.data || []}
            initial={agent}
            onClose={() => setEditing(false)}
            onSubmit={async (values) => {
              await apiPatch<Agent>(`/agents/${agentId}`, token, values);
              setEditing(false);
              agents.refresh();
            }}
          />
        ) : null}

        {deleting && agent ? (
          <ConfirmDialog
            title={`Delete agent “${agent.name}”?`}
            body="This removes the agent, its identity, grants and queued messages. Running work will be orphaned. This cannot be undone."
            confirmText={agent.name}
            onConfirm={async () => {
              await apiDelete(`/agents/${agentId}`, token);
              router.push(`/squads/${squadId}/agents`);
            }}
            onClose={() => setDeleting(false)}
          />
        ) : null}

        {granting ? (
          <GrantModal
            existing={perms.data || []}
            token={token}
            agentId={agentId}
            onClose={() => setGranting(false)}
            onGranted={() => {
              setGranting(false);
              perms.refresh();
            }}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

function ChatThread({
  messages,
  agentName,
  onSent,
  agentId,
  token,
}: {
  messages: Message[];
  agentName: string;
  onSent: () => void;
  agentId: string;
  token: string;
}) {
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const sorted = [...messages].sort((a, b) => (a.created_at || "").localeCompare(b.created_at || ""));

  return (
    <div>
      {sorted.length === 0 ? (
        <EmptyState
          title="No conversation yet"
          hint={`Send a message and ${agentName} will pick it up when it wakes. Replies appear here.`}
        />
      ) : (
        <div className="chat-thread">
          {sorted.map((msg) => {
            const fromUser = msg.from_type === "user";
            return (
              <div key={msg.id} className={`chat-msg ${fromUser ? "from-user" : "from-agent"}`}>
                <div className="chat-who">{fromUser ? "You" : agentName}</div>
                <div className="chat-text">{msg.payload?.message || "(no text)"}</div>
                <div className="chat-time">
                  {formatRelativeTime(msg.created_at)} · {msg.status}
                </div>
              </div>
            );
          })}
        </div>
      )}
      {error ? <div className="notice error">{error}</div> : null}
      <div className="chat-composer">
        <textarea
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder={`Message ${agentName}…`}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <button type="button" className="btn btn-primary" disabled={busy || draft.trim() === ""} onClick={() => void send()}>
          Send
        </button>
      </div>
    </div>
  );

  async function send() {
    if (draft.trim() === "") return;
    setBusy(true);
    setError("");
    try {
      await apiPost(`/agents/${agentId}/chat`, token, { message: draft.trim() });
      setDraft("");
      onSent();
    } catch (err) {
      setError(err instanceof Error ? err.message : "send failed");
    } finally {
      setBusy(false);
    }
  }
}

function GrantModal({
  existing,
  agentId,
  token,
  onClose,
  onGranted,
}: {
  existing: AgentPermission[];
  agentId: string;
  token: string;
  onClose: () => void;
  onGranted: () => void;
}) {
  const [typeIdx, setTypeIdx] = useState(0);
  const [resourceId, setResourceId] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const selectedType = GRANTABLE_TYPES[typeIdx];
  const resources = useApi<RegistryResource[]>(`/registry/${selectedType.path}`, 0);

  const alreadyGranted = (id: string) =>
    existing.some((p) => p.resource_type === selectedType.type && p.resource_id === id);

  return (
    <Modal title="Grant resource access" onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel="Grant"
        submitDisabled={resourceId === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            const next = [
              ...existing.map((p) => ({ resource_type: p.resource_type, resource_id: p.resource_id })),
              { resource_type: selectedType.type, resource_id: resourceId },
            ];
            await apiPut(`/agents/${agentId}/permissions`, token, next);
            onGranted();
          } catch (err) {
            setError(err instanceof Error ? err.message : "grant failed");
            setBusy(false);
          }
        }}
      >
        <label className="field">
          <span>Resource type</span>
          <select
            value={String(typeIdx)}
            onChange={(e) => {
              setTypeIdx(Number(e.target.value));
              setResourceId("");
            }}
          >
            {GRANTABLE_TYPES.map((t, i) => (
              <option key={t.type} value={String(i)}>
                {t.label}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>Resource</span>
          <select value={resourceId} onChange={(e) => setResourceId(e.target.value)}>
            <option value="">— choose one —</option>
            {(resources.data || [])
              .filter((r) => !alreadyGranted(r.id))
              .map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name}
                </option>
              ))}
          </select>
          {resources.error ? <span className="field-hint">{resources.error}</span> : null}
          {(resources.data || []).length === 0 && !resources.loading ? (
            <span className="field-hint">
              No {selectedType.label.toLowerCase()} registered — add some in Settings → Resources.
            </span>
          ) : null}
        </label>
      </ModalForm>
    </Modal>
  );
}
