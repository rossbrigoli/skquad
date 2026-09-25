"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useEffect, useMemo, useRef, useState } from "react";
import { ActivityFeed } from "../../../../../components/ActivityFeed";
import { AgentFormModal } from "../../../../../components/AgentForm";
import { AuthGate } from "../../../../../components/AuthGate";
import { AppShell } from "../../../../../components/AppShell";
import { Collapsible } from "../../../../../components/Collapsible";
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
  type Message,
  type MeteringSummary,
  type RegistryResource,
  type ResourceType,
  type Squad,
  type Task,
} from "../../../../../lib/api";
import { formatCost, formatRelativeTime, formatTokens, leaseState } from "../../../../../lib/format";
import {
  chatContextTokens,
  chatToolCalls,
  formatContextTokens,
  prettyToolArgs,
  sortChatMessages,
  summarizeToolArgs,
  truncateText,
} from "../../../../../lib/chat";
import { agentStatus } from "../../../../../lib/status";
import type { AIModel } from "../../../../../lib/aimodels";
import {
  bindingWarnings,
  buildBindingPayload,
  fallbackChoices,
  findModelById,
  modelLabel,
  selectableModels,
  withCurrentOption,
} from "../../../../../lib/agentLlm";

const GRANTABLE_TYPES: { type: ResourceType; path: string; label: string }[] = [
  { type: "skill", path: "skills", label: "Skills" },
  { type: "tool", path: "tools", label: "Tools" },
  { type: "api", path: "apis", label: "APIs" },
  { type: "knowledge_base", path: "knowledge-bases", label: "Knowledge bases" },
  { type: "project_workspace", path: "project-workspaces", label: "Project workspaces" },
];

// S3776 extractions from AgentProfilePage — presentation split, no behavior change.

function leaseSub(live: Task[], stalled: Task[]): string {
  if (live.length > 0) {
    return live[0].title;
  }
  if (stalled.length > 0) {
    return `${stalled.length} stalled task(s)`;
  }
  return "idle";
}

function TaskListSection({
  title,
  tasks,
  squadId,
  status,
  leaseLabel,
}: Readonly<{
  title: string;
  tasks: Task[];
  squadId: string;
  status: "running" | "stalled";
  leaseLabel: string;
}>) {
  if (tasks.length === 0) {
    return null;
  }
  return (
    <section style={{ marginTop: "var(--space-5)" }}>
      <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>{title}</h2>
      <div className="entity-list">
        {tasks.map((task) => (
          <Link key={task.id} href={`/squads/${squadId}/tasks/${task.id}`} className="entity-row">
            <div className="entity-main">
              <span className="entity-title">{task.title}</span>
              <span className="entity-meta">
                {leaseLabel} {formatRelativeTime(task.lease_expires_at)}
              </span>
            </div>
            <div className="entity-side">
              <StatusChip status={status} />
            </div>
          </Link>
        ))}
      </div>
    </section>
  );
}

function GrantedResourcesSection({
  grants,
  agentId,
  onGrantClick,
  onRevoke,
}: Readonly<{
  grants: AgentPermission[];
  agentId: string;
  onGrantClick: () => void;
  onRevoke: (next: { resource_type: string; resource_id: string }[]) => Promise<void>;
}>) {
  return (
    <section style={{ marginTop: "var(--space-5)" }}>
      <div className="section-head">
        <h2>Granted resources</h2>
        <button type="button" className="btn btn-sm" onClick={onGrantClick}>
          + Grant access
        </button>
      </div>
      {grants.length === 0 ? (
        <EmptyState
          title="No resource grants"
          hint="This agent cannot reach any registered workspaces, APIs or knowledge bases yet."
        />
      ) : (
        <div className="entity-list">
          {grants.map((grant) => (
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
                  onClick={() =>
                    onRevoke(
                      // Rebuild from grants (llm_provider rows excluded): the
                      // backend rejects them on PUT, so replaying legacy rows
                      // would break every revoke.
                      grants
                        .filter((p) => p.id !== grant.id)
                        .map((p) => ({ resource_type: p.resource_type, resource_id: p.resource_id })),
                    ).catch(() => undefined)
                  }
                >
                  Revoke
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function RuntimeIdentitySection({
  hasIdentity,
  busy,
  error,
  onAction,
}: Readonly<{
  hasIdentity: boolean;
  busy: boolean;
  error: string;
  onAction: () => Promise<void>;
}>) {
  return (
    <section style={{ marginTop: "var(--space-5)" }}>
      <div className="section-head">
        <h2>Runtime identity</h2>
        {hasIdentity ? (
          <button type="button" className="btn btn-sm" disabled={busy} onClick={() => onAction().catch(() => undefined)}>
            Rotate identity
          </button>
        ) : (
          <button type="button" className="btn btn-sm btn-primary" disabled={busy} onClick={() => onAction().catch(() => undefined)}>
            Provision identity
          </button>
        )}
      </div>
      {error ? <div className="notice error">{error}</div> : null}
      <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)", margin: 0 }}>
        {hasIdentity
          ? "Identity provisioned — credentials live in the squad namespace as projected secrets. Rotating invalidates the previous credential."
          : "This agent has no runtime identity yet. It cannot connect to the control plane until an identity is provisioned. Bind a primary model in the LLM model section above so provisioning can issue the agent's gateway key."}
      </p>
    </section>
  );
}

export default function AgentProfilePage() {
  const params = useParams<{ id: string; agentId: string }>();
  const squadId = String(params?.id ?? "");
  const agentId = String(params?.agentId ?? "");
  const router = useRouter();
  const { token } = useAuth();

  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 30000);
  const squads = useApi<Squad[]>("/squads");
  const board = useApi<BoardPayload>(`/squads/${squadId}/board`, 15000);
  const metering = useApi<MeteringSummary>(`/agents/${agentId}/metering`, 30000);
  const perms = useApi<AgentPermission[]>(`/agents/${agentId}/permissions`, 60000);
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=50`, 30000);
  const chat = useApi<Message[]>(`/agents/${agentId}/chat`, 10000);
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
  // WP2 (ADR-0010) made llm_provider grants legacy: they are no longer
  // grantable and the LLM tab supersedes them, so hide any surviving
  // llm_provider rows from the permissions surface (WP8 drops the type).
  const resourceGrants = (perms.data || []).filter((p) => p.resource_type !== "llm_provider");

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
            sub={leaseSub(live, stalled)}
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
            value={resourceGrants.length}
            sub={resourceGrants.length === 0 ? "no resource grants" : "see below"}
          />
        </div>

        <TaskListSection title="Working on" tasks={live} squadId={squadId} status="running" leaseLabel="lease expires" />

        <TaskListSection title="Stalled work" tasks={stalled} squadId={squadId} status="stalled" leaseLabel="lease expired" />

        <section style={{ marginTop: "var(--space-5)" }}>
          <div className="section-head">
            <h2>Talk to {agent?.name || "this agent"}</h2>
          </div>
          <ChatThread messages={chat.data || []} agentName={agent?.name || "agent"} onSent={() => chat.refresh()} agentId={agentId} token={token} />
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <div className="section-head">
            <h2>LLM model</h2>
          </div>
          {agent ? (
            <LlmBindingSection
              key={`${agent.id}:${agent.ai_model_id ?? ""}:${agent.fallback_ai_model_id ?? ""}`}
              agentId={agentId}
              token={token}
              primaryId={agent.ai_model_id ?? ""}
              fallbackId={agent.fallback_ai_model_id ?? ""}
              onSaved={() => agents.refresh()}
            />
          ) : (
            <div className="notice">Loading agent…</div>
          )}
        </section>

        <GrantedResourcesSection
          grants={resourceGrants}
          agentId={agentId}
          onGrantClick={() => setGranting(true)}
          onRevoke={async (next) => {
            await apiPut(`/agents/${agentId}/permissions`, token, next);
            perms.refresh();
          }}
        />

        <RuntimeIdentitySection
          hasIdentity={!!agent?.identity_id}
          busy={identityBusy}
          error={identityError}
          onAction={async () => {
            setIdentityBusy(true);
            setIdentityError("");
            try {
              await apiPost(agent?.identity_id ? `/agents/${agentId}/identity/rotate` : `/agents/${agentId}/identity`, token, {});
              agents.refresh();
            } catch (err) {
              const fallbackMsg = agent?.identity_id ? "rotate failed" : "provision failed";
              setIdentityError(err instanceof Error ? err.message : fallbackMsg);
            } finally {
              setIdentityBusy(false);
            }
          }}
        />

        {/* S-120: collapsed by default, reusing the S-118 Collapsible with a
            session key distinct from the squad page's key. */}
        <Collapsible id="agent-recent-activity" title="Recent activity">
          <ActivityFeed
            squadId={squadId}
            entries={agentActivity}
            emptyTitle="No recorded activity for this agent"
            emptyHint="Assignments, status changes and identity events show up here."
          />
        </Collapsible>

        {editing && agent ? (
          <AgentFormModal
            title={`Edit ${agent.name}`}
            submitLabel="Save changes"
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
            existing={resourceGrants}
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

// Initials for chat avatars: first + last initial of a name (S-105).
function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
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
  const { user } = useAuth();
  const scrollRef = useRef<HTMLDivElement>(null);

  const sorted = useMemo(() => sortChatMessages(messages), [messages]);
  // S-122: tiny context-size bar — the prompt-token count the runtime
  // recorded on the most recent agent reply (the real context size of the
  // last LLM call, not an estimate).
  const contextTokens = useMemo(() => chatContextTokens(messages), [messages]);

  // Keep the newest message in view (S-105).
  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [sorted.length]);

  function autoGrow(node: HTMLTextAreaElement | null) {
    if (!node) return;
    node.style.height = "auto";
    node.style.height = `${Math.min(node.scrollHeight, 160)}px`;
  }

  return (
    <div className="chat">
      <div className="chat-scroll" ref={scrollRef}>
        {sorted.length === 0 ? (
          <div className="chat-empty">
            <div className="chat-avatar agent">{initials(agentName)}</div>
            <div className="chat-empty-title">{agentName} is listening</div>
            <p className="chat-empty-hint">
              Send a message below. If the agent is asleep it will pick your message up when it
              wakes; replies appear right here.
            </p>
          </div>
        ) : (
          sorted.map((msg) => {
            const fromUser = msg.from_type === "user";
            const toolCalls = fromUser ? [] : chatToolCalls(msg);
            return (
              <div key={msg.id} className={`chat-row ${fromUser ? "mine" : "theirs"}`}>
                <div className={`chat-avatar ${fromUser ? "me" : "agent"}`} aria-hidden="true">
                  {fromUser ? initials(user?.name ?? "You") : initials(agentName)}
                </div>
                <div className="chat-bubble">
                  <div className="chat-head">
                    <span className="chat-name">{fromUser ? "You" : agentName}</span>
                    <span className="chat-time">{formatRelativeTime(msg.created_at)}</span>
                    {!fromUser && msg.status ? <span className="chat-status mono">{msg.status}</span> : null}
                  </div>
                  <div className="chat-text">{msg.payload?.message || "(no text)"}</div>
                  {toolCalls.length > 0 ? (
                    <div className="chat-tools">
                      {toolCalls.map((call, idx) => (
                        <details key={`${call.name}-${idx}`} className={`chat-tool${call.ok ? "" : " failed"}`}>
                          <summary className="chat-tool-summary">
                            <span className="chat-tool-name">🔧 {call.name}</span>
                            {summarizeToolArgs(call.arguments) ? (
                              <span className="chat-tool-args mono">{summarizeToolArgs(call.arguments)}</span>
                            ) : null}
                            <span className="chat-tool-state">{call.ok ? "ok" : "failed"}</span>
                          </summary>
                          <pre className="chat-tool-detail mono">{prettyToolArgs(call.arguments)}</pre>
                          {call.result ? (
                            <pre className="chat-tool-detail mono result">{truncateText(call.result, 4000)}</pre>
                          ) : null}
                        </details>
                      ))}
                    </div>
                  ) : null}
                </div>
              </div>
            );
          })
        )}
      </div>
      <output className="chat-context-bar" aria-live="polite">
        {contextTokens === null
          ? "context: — tokens (waiting for the agent's first reply)"
          : `context ≈ ${formatContextTokens(contextTokens)} tokens · last agent turn`}
      </output>
      {error ? <div className="notice error" style={{ margin: "var(--space-2) var(--space-3) 0" }}>{error}</div> : null}
      <form
        className="chat-composer"
        onSubmit={(e) => {
          e.preventDefault();
          send();
        }}
      >
        <textarea
          value={draft}
          rows={1}
          onChange={(e) => {
            setDraft(e.target.value);
            autoGrow(e.target);
          }}
          placeholder={`Message ${agentName}…`}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              send();
            }
          }}
        />
        <button
          type="submit"
          className="chat-send"
          disabled={busy || draft.trim() === ""}
          aria-label="Send message"
          title="Send"
        >
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
            <path d="M2 14L14 2M14 2H5M14 2V11" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
        </button>
      </form>
      <div className="chat-hint">Enter to send · Shift+Enter for a new line</div>
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

// WP7 (S-112) — LLM model binding section (ADR-0010 D4): primary is
// required, fallback optional. Options come from the caller's granted+
// active models (/models/me); the selected primary is excluded from the
// fallback list. Warnings are soft — Save is never disabled by them.
function LlmBindingSection({
  agentId,
  token,
  primaryId,
  fallbackId,
  onSaved,
}: {
  readonly agentId: string;
  readonly token: string;
  readonly primaryId: string;
  readonly fallbackId: string;
  readonly onSaved: () => void;
}) {
  const myModels = useApi<AIModel[]>("/models/me", 60000);
  const [primary, setPrimary] = useState(primaryId);
  const [fallback, setFallback] = useState(fallbackId);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);

  const models = myModels.data ?? [];
  const primaryOpts = withCurrentOption(selectableModels(models), models, primary);
  const fallbackOpts = fallbackChoices(models, primary);
  const primaryModel = findModelById(models, primary);
  const fallbackModel = findModelById(models, fallback);
  const warnings = bindingWarnings(primaryModel, fallbackModel);
  const dirty = primary !== primaryId || fallback !== fallbackId;

  async function save() {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      await apiPatch(`/agents/${agentId}`, token, buildBindingPayload(primary, fallback));
      setSaved(true);
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "save failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ display: "grid", gap: "var(--space-3)" }}>
      {myModels.error ? <div className="notice error">{myModels.error}</div> : null}
      {!myModels.loading && models.length === 0 ? (
        <div className="notice">
          You have no granted AI Models. Ask a platform admin to grant you models in Settings → Access
          before binding this agent.
        </div>
      ) : null}
      <div className="field-row">
        <label className="field">
          <span>Primary model (required)</span>
          <select
            value={primary}
            onChange={(e) => {
              setPrimary(e.target.value);
              setSaved(false);
              // A model cannot be its own fallback — clear it if it collides.
              if (e.target.value === fallback) setFallback("");
            }}
          >
            <option value="">— choose a model —</option>
            {primaryOpts.models.map((m) => (
              <option key={m.id} value={m.id}>
                {modelLabel(m)}
              </option>
            ))}
            {primaryOpts.staleCurrent ? (
              <option value={primary}>{`current binding (${primary.slice(0, 12)}…)`}</option>
            ) : null}
          </select>
          {primaryOpts.staleCurrent ? (
            <span className="field-hint">
              The current binding is no longer granted/active for you — pick a new model to rebind.
            </span>
          ) : null}
        </label>
        <label className="field">
          <span>Fallback model (optional)</span>
          <select
            value={fallback}
            onChange={(e) => {
              setFallback(e.target.value);
              setSaved(false);
            }}
          >
            <option value="">— none —</option>
            {fallbackOpts.map((m) => (
              <option key={m.id} value={m.id}>
                {modelLabel(m)}
              </option>
            ))}
          </select>
          {primary && fallbackOpts.length === 0 && !myModels.loading ? (
            <span className="field-hint">No other granted model available for fallback.</span>
          ) : null}
        </label>
      </div>

      {fallback && warnings.length > 0 ? (
        <div style={{ display: "grid", gap: "var(--space-2)" }}>
          {warnings.map((w) => (
            <div key={w.code} className="notice warn">
              ⚠ {w.message}
            </div>
          ))}
        </div>
      ) : null}

      <div style={{ display: "flex", alignItems: "center", gap: "var(--space-3)" }}>
        <button
          type="button"
          className="btn btn-sm btn-primary"
          disabled={busy || primary === ""}
          onClick={() => {
            save();
          }}
        >
          {busy ? "Saving…" : "Save binding"}
        </button>
        {saved && !dirty ? <span className="field-hint">Binding saved.</span> : null}
        <span className="field-hint">
          Changing the binding re-provisions the agent&apos;s gateway key immediately.
        </span>
      </div>
      {error ? <div className="notice error">{error}</div> : null}
    </div>
  );
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
