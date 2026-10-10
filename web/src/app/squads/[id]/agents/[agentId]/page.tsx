"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ActivityFeed } from "../../../../../components/ActivityFeed";
import { AuthGate } from "../../../../../components/AuthGate";
import { AppShell } from "../../../../../components/AppShell";
import { Collapsible } from "../../../../../components/Collapsible";
import { ConfirmDialog } from "../../../../../components/ConfirmDialog";
import { EmptyState } from "../../../../../components/EmptyState";
import { MarkdownMessage } from "../../../../../components/MarkdownMessage";
import { EffectivePromptPanel } from "../../../../../components/EffectivePromptPanel";
import { PromptRevisionsPanel } from "../../../../../components/PromptRevisionsPanel";
import { PromptTierEditor } from "../../../../../components/PromptTierEditor";
import { Modal, ModalForm } from "../../../../../components/Modal";
import { StatusChip } from "../../../../../components/StatusChip";
import { DEFAULT_AGENT_STORAGE_SIZE, STORAGE_PRESETS, isValidStorageSize, storageDisplay } from "../../../../../lib/agentStorage";
import { chatSenderAttribution } from "../../../../../lib/actorDisplay";
import { monthStartISO } from "../../../../../lib/metering";
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
  type Task,
} from "../../../../../lib/api";
import { formatCost, formatRelativeTime, leaseState } from "../../../../../lib/format";
import {
  agentTurnPending,
  chatContextTokens,
  chatToolCalls,
  formatContextTokens,
  isTurnErrorMessage,
  prettyToolArgs,
  sortChatMessages,
  subagentSummary,
  summarizeToolArgs,
  truncateText,
  uniqueContentKeys,
  type ChatToolCall,
  type SubagentInfo,
} from "../../../../../lib/chat";
import { SubagentThreadPanel } from "../../../../../components/SubagentThreadPanel";
import { AgentInboxPanel } from "../../../../../components/AgentInboxPanel";
import { AttachmentThumbs } from "../../../../../components/AttachmentThumbs";
import { MAX_MESSAGE_ATTACHMENTS, extractPastedImages, messageAttachments, validateImageFile, type UploadRef } from "../../../../../lib/uploads";
import { apiUploadImage } from "../../../../../lib/api";
import { agentStatus } from "../../../../../lib/status";
import { THINKING_LEVELS, resolveThinkingLevel, thinkingLevelLabel, type ThinkingLevel } from "../../../../../lib/thinking";
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
import { foldRestConstraints, grantConstraintsLabel, restCeilingSummary, splitList, type RestCeiling } from "../../../../../lib/restResources";
import { RestAgentCredential } from "../../../../../components/RestAgentCredential";
import { GitAgentCredential } from "../../../../../components/GitAgentCredential";

const GRANTABLE_TYPES: { type: ResourceType; path: string; label: string }[] = [
  { type: "skill", path: "skills", label: "Skills" },
  { type: "tool", path: "tools", label: "Tools" },
  { type: "api", path: "apis", label: "APIs" },
  { type: "git", path: "git", label: "Git repos" },
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
  token,
  onGrantClick,
  onRevoke,
}: Readonly<{
  grants: AgentPermission[];
  agentId: string;
  token: string;
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
                {grantConstraintsLabel(grant.constraints) ? (
                  <span className="entity-meta">narrowing: {grantConstraintsLabel(grant.constraints)}</span>
                ) : null}
                {["api", "rest"].includes(grant.resource_type as string) ? (
                  // TG-4c (S-259): own-vs-default credential per agent.
                  // "api" is the legacy alias the web types still use.
                  <RestAgentCredential resourceId={grant.resource_id} agentId={agentId} token={token} />
                ) : null}
                {grant.resource_type === ("git" as string) ? (
                  // TG-4b: same own-vs-default custody for git grants
                  // (bearer PAT, skquad-git- Secret prefix).
                  <GitAgentCredential resourceId={grant.resource_id} agentId={agentId} token={token} />
                ) : null}
              </div>
              <div className="entity-side">
                <span className="entity-meta">{formatRelativeTime(grant.created_at)}</span>
                <button
                  type="button"
                  className="btn btn-sm btn-danger"
                  onClick={() =>
                    onRevoke(
                      // Rebuild from grants (ai_provider rows excluded): the
                      // backend rejects them on PUT, so replaying legacy rows
                      // would break every revoke.
                      grants
                        .filter((p) => p.id !== grant.id)
                        .map((p) => ({
                          resource_type: p.resource_type,
                          resource_id: p.resource_id,
                          // TG-4: preserve constraints — the PUT
                          // replaces the whole set.
                          ...(p.constraints && Object.keys(p.constraints as object).length > 0 ? { constraints: p.constraints } : {}),
                        })),
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
            Provision identity now
          </button>
        )}
      </div>
      {error ? <div className="notice error">{error}</div> : null}
      <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)", margin: 0 }}>
        {hasIdentity
          ? "Identity provisioned — credentials live in the squad namespace as projected secrets. Rotating invalidates the previous credential."
          : "This agent predates auto-provisioning (S-170): new agents get their identity automatically at creation. This one needs a one-time provision — make sure a primary model is bound above, then use the button."}
      </p>
    </section>
  );
}

// S-189/S4323: agent profile page tabs.
type AgentTab = "chat" | "config" | "inbox";

// S-189/S3776: pure helpers extracted from AgentProfilePage so its
// cognitive complexity stays under the limit.
function describeLlmBinding(models: AIModel[], id: string | undefined): string {
  if (!id) {
    return "no model bound";
  }
  const m = findModelById(models, id);
  return m ? modelLabel(m) : `${id.slice(0, 12)}…`;
}

function errMsg(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

// S-189/S3776: title row + meta line extracted from AgentProfilePage;
// render output identical to the inline JSX.
function AgentTitleRow({
  agent,
  agentId,
  restartBusy,
  restartNote,
  onConfirmRestart,
  live,
  stalled,
  tasks,
  resourceGrants,
  metering,
  meteringMtd,
  contextTokens,
}: {
  readonly agent: Agent | undefined;
  readonly agentId: string;
  readonly restartBusy: boolean;
  readonly restartNote: string;
  readonly onConfirmRestart: () => void;
  readonly live: Task[];
  readonly stalled: Task[];
  readonly tasks: Task[];
  readonly resourceGrants: AgentPermission[];
  readonly metering: MeteringState;
  readonly meteringMtd: MeteringState;
  readonly contextTokens: number | null;
}) {
  return (
    <>
      <div className="section-head agent-title-row">
        <div style={{ display: "flex", alignItems: "baseline", gap: "var(--space-3)", flexWrap: "wrap" }}>
          <h1 className="page-title" style={{ margin: 0 }}>
            {agent?.name ?? "Agent"}
          </h1>
          {agent ? <StatusChip status={agentStatus(agent)} /> : null}
          {/* S-169 item 9b: restart lives beside the page title now. */}
          <button
            type="button"
            className="btn btn-sm btn-danger"
            disabled={!agent || restartBusy}
            onClick={onConfirmRestart}
          >
            {restartBusy ? "Restarting…" : "Restart agent"}
          </button>
          {restartNote ? <span className="field-hint">{restartNote}</span> : null}
        </div>
        {/* S-178: the metric chips moved up into the title row, where
            the Delete button used to sit. Delete itself moved to the
            Configuration tab's Danger zone — beside the chat it read
            like a message-delete. */}
        <AgentMetricChips
          live={live}
          stalled={stalled}
          tasks={tasks}
          resourceGrants={resourceGrants}
          metering={metering}
          meteringMtd={meteringMtd}
          agent={agent}
          contextTokens={contextTokens}
        />
      </div>
      <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)" }}>
        {agent?.role ? agent.role : "no role set"} · <span className="mono">{agentId.slice(0, 12)}</span>
      </p>
    </>
  );
}

export default function AgentProfilePage() {
  const params = useParams<{ id: string; agentId: string }>();
  const squadId = String(params?.id ?? "");
  const agentId = String(params?.agentId ?? "");
  const router = useRouter();
  const { token } = useAuth();

  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 30000);
  const board = useApi<BoardPayload>(`/squads/${squadId}/board`, 15000);
  const metering = useApi<MeteringSummary>(`/agents/${agentId}/metering`, 30000);
  // S-169 item 10: month-to-date spend — same endpoint, windowed by the
  // viewer's local month start (CP gained ?since= for this).
  const meteringMtd = useApi<MeteringSummary>(`/agents/${agentId}/metering?since=${monthStartISO()}`, 30000);
  const perms = useApi<AgentPermission[]>(`/agents/${agentId}/permissions`, 60000);
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=50`, 30000);
  const chat = useApi<Message[]>(`/agents/${agentId}/chat`, 10000);
  // S-169: Talk-first layout — Chat is the default tab, Configuration holds
  // everything the old Edit modal + stacked sections carried.
  // S-174: Inbox adds the delivery-queue observability panel (owner/admin).
  // S-189/S4323: named union for the tab state.
  const [tab, setTab] = useState<AgentTab>("chat");
  const [deleting, setDeleting] = useState(false);
  // S-202: restart/reset go through the shared ConfirmDialog instead of
  // window.confirm, matching every other destructive action in the app.
  const [confirmReset, setConfirmReset] = useState(false);
  const [confirmRestart, setConfirmRestart] = useState(false);
  const [granting, setGranting] = useState(false);
  const [identityBusy, setIdentityBusy] = useState(false);
  const [identityError, setIdentityError] = useState("");
  const [previewEffective, setPreviewEffective] = useState(false);
  // Bumped after a successful config save so the form remounts with the
  // server's fresh values — no manual state reconciliation.
  const [savedTick, setSavedTick] = useState(0);
  // S-162 actions relocated by S-169 items 9a/9b: reset lives in the chat
  // header now, restart sits next to the page title.
  const [chatActionBusy, setChatActionBusy] = useState(false);
  const [chatNote, setChatNote] = useState("");
  const [restartBusy, setRestartBusy] = useState(false);
  const [restartNote, setRestartNote] = useState("");
  // S-178: thinking-level selector in the composer (immediate PATCH, not
  // part of the Configuration form).
  const [thinkingBusy, setThinkingBusy] = useState(false);
  // S-178: resolve the agent's bound model for the composer's model chip.
  const myModels = useApi<AIModel[]>("/models/me", 60000);

  async function saveThinkingLevel(level: ThinkingLevel) {
    setThinkingBusy(true);
    try {
      await apiPatch(`/agents/${agentId}`, token, { thinking_level: level });
      agents.refresh();
    } finally {
      setThinkingBusy(false);
    }
  }

  const agent = (agents.data || []).find((a) => a.id === agentId);
  const tasks = (board.data?.tasks || []).filter((t) => t.assignee_agent_id === agentId);
  const live = tasks.filter((t) => leaseState(t) === "running");
  const stalled = tasks.filter((t) => leaseState(t) === "stalled");
  const agentActivity = (audit.data || []).filter((e) => e.resource_id === agentId);
  // WP2 (ADR-0010) made ai_provider grants legacy: they are no longer
  // grantable and the LLM tab supersedes them, so hide any surviving
  // ai_provider rows from the permissions surface (WP8 drops the type).
  const resourceGrants = (perms.data || []).filter((p) => p.resource_type !== "ai_provider");
  // S-169 item 12: the context-window stat moved from under the chat box
  // up into the compact chip row.
  const contextTokens = useMemo(() => chatContextTokens(chat.data || []), [chat.data]);
  // S-178: human-readable name of the agent's bound LLM for the composer
  // chip; falls back to the bare id when the model is no longer visible
  // via /models/me (revoked/deprecated).
  const llmLabel = useMemo(
    () => describeLlmBinding(myModels.data ?? [], agent?.ai_model_id),
    [agent?.ai_model_id, myModels.data],
  );

  async function resetChat() {
    setChatActionBusy(true);
    setChatNote("");
    try {
      const res = await apiPost<{ archived: number }>(`/agents/${agentId}/chat/reset`, token, {});
      setChatNote(`Thread reset · ${res?.archived ?? 0} earlier message(s) archived to memory`);
      chat.refresh();
    } catch (err) {
      setChatNote(errMsg(err, "reset failed"));
    } finally {
      setChatActionBusy(false);
    }
  }

  async function restartAgent() {
    setRestartBusy(true);
    setRestartNote("");
    try {
      const res = await apiPost<{ pods: number }>(`/agents/${agentId}/restart`, token, {});
      setRestartNote(`Restarting agent (${res?.pods ?? 0} pod(s) evicted)`);
    } catch (err) {
      setRestartNote(errMsg(err, "restart failed"));
    } finally {
      setRestartBusy(false);
    }
  }

  if (!agent && !agents.loading) {
    return (
      <AuthGate>
        <AppShell>
          <EmptyState title="Agent not found" hint="It may have been removed from this squad." />
        </AppShell>
      </AuthGate>
    );
  }

  return (
    <AuthGate>
      <AppShell>
        <AgentTitleRow
          agent={agent}
          agentId={agentId}
          restartBusy={restartBusy}
          restartNote={restartNote}
          onConfirmRestart={() => setConfirmRestart(true)}
          live={live}
          stalled={stalled}
          tasks={tasks}
          resourceGrants={resourceGrants}
          metering={metering}
          meteringMtd={meteringMtd}
          contextTokens={contextTokens}
        />

        <AgentSectionTabs tab={tab} onSelect={setTab} />

        {tab === "inbox" ? <AgentInboxPanel agentId={agentId} /> : null}

        {tab === "chat" ? (
          <section className="agent-chat-section" style={{ marginTop: "var(--space-4)" }}>
            <div className="section-head">
              <h2>Talk to {agent?.name ?? "this agent"}</h2>
              {/* S-169 item 9a: Reset chat sits in the chat header now. */}
              <button
                type="button"
                className="btn btn-sm btn-danger"
                disabled={chatActionBusy || (chat.data || []).length === 0}
                onClick={() => setConfirmReset(true)}
              >
                Reset chat
              </button>
            </div>
            {chatNote ? <p className="field-hint">{chatNote}</p> : null}
            <ChatThread
              messages={chat.data || []}
              agentName={agent?.name ?? "agent"}
              onSent={() => chat.refresh()}
              agentId={agentId}
              squadId={squadId}
              token={token}
              llmLabel={llmLabel}
              thinkingLevel={resolveThinkingLevel(agent?.thinking_level)}
              thinkingBusy={thinkingBusy}
              onThinkingLevelChange={saveThinkingLevel}
              // S-185: task lists ride inside the chat scroll area (they
              // used to sit below the chat and push the page past the
              // fold). S-169's "reachable without crowding" intent holds.
              footer={
                <>
                  <TaskListSection title="Working on" tasks={live} squadId={squadId} status="running" leaseLabel="lease expires" />
                  <TaskListSection title="Stalled work" tasks={stalled} squadId={squadId} status="stalled" leaseLabel="lease expired" />
                </>
              }
            />
          </section>
        ) : null}

        {tab === "config" && agent ? (
          <AgentConfigPane
            key={`${agentId}:${savedTick}`}
            agent={agent}
            token={token}
            squadId={squadId}
            grants={resourceGrants}
            activityEntries={agentActivity}
            identityBusy={identityBusy}
            identityError={identityError}
            onGrantClick={() => setGranting(true)}
            onRevoke={async (next) => {
              await apiPut(`/agents/${agentId}/permissions`, token, next);
              perms.refresh();
            }}
            onIdentityAction={async () => {
              setIdentityBusy(true);
              setIdentityError("");
              try {
                await apiPost(agent?.identity_id ? `/agents/${agentId}/identity/rotate` : `/agents/${agentId}/identity`, token, {});
                agents.refresh();
              } catch (err) {
                const fallbackMsg = agent?.identity_id ? "rotate failed" : "provision failed";
                setIdentityError(errMsg(err, fallbackMsg));
              } finally {
                setIdentityBusy(false);
              }
            }}
            onPreviewEffective={() => setPreviewEffective(true)}
            onRequestDelete={() => setDeleting(true)}
            onSaved={() => {
              setSavedTick((t) => t + 1);
              agents.refresh();
              perms.refresh();
              metering.refresh();
              meteringMtd.refresh();
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

        {confirmReset ? (
          <ConfirmDialog
            title="Reset this chat thread?"
            body="Earlier turns stop being included in the agent's context. The transcript is saved to the agent's memory."
            confirmLabel="Reset chat"
            onConfirm={async () => {
              setConfirmReset(false);
              await resetChat();
            }}
            onClose={() => setConfirmReset(false)}
          />
        ) : null}

        {confirmRestart ? (
          <ConfirmDialog
            title="Restart the agent?"
            body="Its pod is evicted and a fresh one starts. Use this if the agent seems stuck."
            confirmLabel="Restart agent"
            onConfirm={async () => {
              setConfirmRestart(false);
              await restartAgent();
            }}
            onClose={() => setConfirmRestart(false)}
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

        {previewEffective && agent ? (
          <Modal title={`Effective prompt — ${agent.name}`} wide onClose={() => setPreviewEffective(false)}>
            <EffectivePromptPanel agentId={agentId} />
          </Modal>
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

// S-169: the Configuration tab. The old Edit-modal fields are inlined as
// sections with ONE Save for the whole form (items 5–7): role, idle
// timeout, storage, agent prompt and the LLM binding all ride a single
// PATCH /agents/{id}. Grants and runtime identity stay immediate-action
// sections (they were never part of the modal); recent activity closes the
// tab.
function AgentConfigPane({
  agent,
  token,
  squadId,
  grants,
  activityEntries,
  identityBusy,
  identityError,
  onGrantClick,
  onRevoke,
  onIdentityAction,
  onPreviewEffective,
  onRequestDelete,
  onSaved,
}: {
  readonly agent: Agent;
  readonly token: string;
  readonly squadId: string;
  readonly grants: AgentPermission[];
  readonly activityEntries: AuditEntry[];
  readonly identityBusy: boolean;
  readonly identityError: string;
  readonly onGrantClick: () => void;
  readonly onRevoke: (next: { resource_type: string; resource_id: string }[]) => Promise<void>;
  readonly onIdentityAction: () => Promise<void>;
  readonly onPreviewEffective: () => void;
  readonly onRequestDelete: () => void;
  readonly onSaved: () => void;
}) {
  const [role, setRole] = useState(agent.role ?? "");
  // S-183: empty = follow the platform idle scale-to-zero setting (0).
  const [idleTimeout, setIdleTimeout] = useState(
    agent.idle_timeout_sec ? String(agent.idle_timeout_sec) : "",
  );
  const [storageEnabled, setStorageEnabled] = useState(agent.storage_enabled ?? false);
  const [storageSize, setStorageSize] = useState(agent.storage_size ? agent.storage_size : DEFAULT_AGENT_STORAGE_SIZE);
  const [prompt, setPrompt] = useState(agent.system_prompt ?? "");
  const [primary, setPrimary] = useState(agent.ai_model_id ?? "");
  const [fallback, setFallback] = useState(agent.fallback_ai_model_id ?? "");
  const [promptBlocked, setPromptBlocked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [savedNote, setSavedNote] = useState("");

  const storageInvalid = storageEnabled && !isValidStorageSize(storageSize);
  const storageBaseline = agent.storage_size ? agent.storage_size : DEFAULT_AGENT_STORAGE_SIZE;
  const storageSizeDirty = storageEnabled && storageSize.trim() !== storageBaseline;
  const dirty =
    role !== (agent.role ?? "") ||
    prompt !== (agent.system_prompt ?? "") ||
    Number(idleTimeout || 0) !== (agent.idle_timeout_sec ?? 0) ||
    storageEnabled !== (agent.storage_enabled ?? false) ||
    storageSizeDirty ||
    primary !== (agent.ai_model_id ?? "") ||
    fallback !== (agent.fallback_ai_model_id ?? "");

  async function saveAll() {
    setBusy(true);
    setError("");
    setSavedNote("");
    try {
      // buildBindingPayload throws on an empty/colliding primary — caught
      // here and surfaced like any other save error.
      const binding = buildBindingPayload(primary, fallback);
      const body: Record<string, unknown> = {
        role,
        system_prompt: prompt,
        idle_timeout_sec: idleTimeout.trim() === "" ? 0 : Number(idleTimeout),
        storage_enabled: storageEnabled,
        storage_size: storageEnabled ? storageSize.trim() : "",
        ai_model_id: binding.ai_model_id,
        fallback_ai_model_id: binding.fallback_ai_model_id,
      };
      await apiPatch(`/agents/${agent.id}`, token, body);
      setSavedNote("Configuration saved.");
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "save failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="config-pane">
      <section style={{ marginTop: "var(--space-4)" }}>
        <div className="section-head">
          <h2>Basics</h2>
        </div>
        <div className="field-row">
          <label className="field">
            <span>Name</span>
            <input value={agent.name} readOnly />
            <span className="field-hint">
              Names are immutable — the Kubernetes deployment name is derived from it.
            </span>
          </label>
          <label className="field">
            <span>Role</span>
            <input
              value={role}
              onChange={(e) => {
                setRole(e.target.value);
                setSavedNote("");
              }}
              placeholder="e.g. implementer"
            />
          </label>
          <label className="field">
            <span>Idle timeout (sec)</span>
            <input
              value={idleTimeout}
              onChange={(e) => {
                setIdleTimeout(e.target.value.replace(/\D/g, ""));
                setSavedNote("");
              }}
              inputMode="numeric"
              placeholder="Platform default (15 min)"
            />
          </label>
        </div>
        <StorageWorkspaceField
          storageEnabled={storageEnabled}
          storageSize={storageSize}
          storageInvalid={storageInvalid}
          onChangeEnabled={(v) => {
            setStorageEnabled(v);
            setSavedNote("");
          }}
          onChangeSize={(v) => {
            setStorageSize(v);
            setSavedNote("");
          }}
        />
      </section>

      <section style={{ marginTop: "var(--space-5)" }}>
        <div className="section-head">
          <h2>LLM model</h2>
        </div>
        <LlmBindingFields
          primary={primary}
          fallback={fallback}
          onPrimaryChange={(id) => {
            setPrimary(id);
            // A model cannot be its own fallback — clear it if it collides.
            if (id === fallback) setFallback("");
            setSavedNote("");
          }}
          onFallbackChange={(id) => {
            setFallback(id);
            setSavedNote("");
          }}
        />
      </section>

      <section style={{ marginTop: "var(--space-5)" }}>
        <div className="section-head">
          <h2>Agent Context</h2>
          <button type="button" className="btn btn-sm" onClick={onPreviewEffective}>
            Preview effective prompt
          </button>
        </div>
        <p className="field-hint" style={{ marginBottom: "var(--space-3)" }}>
          The agent prompt is layer 4 — your agent&rsquo;s identity and personality. It cannot
          relax anything in the platform, organization, or squad layers above it.
        </p>
        <PromptTierEditor
          scope="agent"
          label="Agent prompt"
          content={prompt}
          onChange={(v) => {
            setPrompt(v);
            setSavedNote("");
          }}
          onSave={saveAll}
          showSaveButton={false}
          onBlockedChange={setPromptBlocked}
        />
        <div style={{ marginTop: "var(--space-4)" }}>
          <PromptRevisionsPanel
            scope="agent"
            scopeId={agent.id}
            onRestore={(content) => {
              setPrompt(content);
              setSavedNote("");
            }}
          />
        </div>
      </section>

      <div className="config-save-bar">
        <button
          type="button"
          className="btn btn-primary"
          disabled={!dirty || busy || promptBlocked || storageInvalid || primary === ""}
          onClick={() => {
            void saveAll();
          }}
        >
          {busy ? "Saving…" : "Save configuration"}
        </button>
        {savedNote && !dirty ? <span className="field-hint">{savedNote}</span> : null}
        {promptBlocked ? <span className="entity-meta">Fix the prompt errors above before saving.</span> : null}
        {storageInvalid ? <span className="entity-meta">Storage size is invalid.</span> : null}
        {primary === "" ? <span className="entity-meta">A primary model is required.</span> : null}
      </div>
      {error ? (
        <div className="notice error" role="alert">
          {error}
        </div>
      ) : null}

      <GrantedResourcesSection grants={grants} agentId={agent.id} token={token} onGrantClick={onGrantClick} onRevoke={onRevoke} />

      <RuntimeIdentitySection
        hasIdentity={!!agent.identity_id}
        busy={identityBusy}
        error={identityError}
        onAction={onIdentityAction}
      />

      {/* S-120: collapsed by default, reusing the S-118 Collapsible with a
          session key distinct from the squad page's key. */}
      <Collapsible id="agent-recent-activity" title="Recent activity">
        <ActivityFeed
          squadId={squadId}
          entries={activityEntries}
          emptyTitle="No recorded activity for this agent"
          emptyHint="Assignments, status changes and identity events show up here."
        />
      </Collapsible>

      {/* S-178: Danger zone — the destructive delete moved here from the
          chat screen header, where it read like a per-message action.
          The ConfirmDialog (typed-name confirmation) still gates it. */}
      <section className="danger-zone" aria-label="Danger zone">
        <h2>Danger zone</h2>
        <div className="danger-zone-row">
          <div>
            <div className="danger-zone-title">Delete this agent</div>
            <p className="field-hint" style={{ margin: 0 }}>
              Removes the agent, its identity, grants and queued messages.
              Running work will be orphaned. This cannot be undone.
            </p>
          </div>
          <button type="button" className="btn btn-danger" onClick={onRequestDelete}>
            Delete agent
          </button>
        </div>
      </section>
    </div>
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
  squadId,
  token,
  llmLabel,
  thinkingLevel,
  thinkingBusy,
  onThinkingLevelChange,
  footer,
}: {
  readonly messages: Message[];
  readonly agentName: string;
  readonly onSent: () => void;
  readonly agentId: string;
  readonly squadId: string;
  readonly token: string;
  // S-178: model info + thinking level surfaced at the composer.
  readonly llmLabel: string;
  readonly thinkingLevel: ThinkingLevel;
  readonly thinkingBusy: boolean;
  readonly onThinkingLevelChange: (level: ThinkingLevel) => Promise<void>;
  // S-185: extra content (task lists) rendered inside the chat scroll
  // area so the page itself never scrolls past the chat viewport.
  readonly footer?: ReactNode;
}) {
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // S-175: stop button — suppress the pending-turn indicator from the
  // moment cancel is requested, without waiting for the chat poll to
  // catch the new "cancelled" status. Cleared on the next send.
  const [stopRequested, setStopRequested] = useState(false);
  const [cancelBusy, setCancelBusy] = useState(false);
  // S-163: subagent transparency — the thread shown in the side panel.
  const [openSubagent, setOpenSubagent] = useState<SubagentInfo | null>(null);
  // S-194: image attachments staged in the composer (uploaded, not yet
  // sent) and the file picker input driven by the paperclip / paste.
  const [pendingAttachments, setPendingAttachments] = useState<UploadRef[]>([]);
  const [uploadBusy, setUploadBusy] = useState(false);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const { user } = useAuth();
  const scrollRef = useRef<HTMLDivElement>(null);
  // S-175: auto-scroll only while the user is already near the bottom, so
  // reading history never gets yanked around. Set on scroll, forced on send.
  const stickToBottom = useRef(true);
  // S-154: throttle for the wake-on-typing ping (once a minute max).
  const lastWakeAt = useRef(0);

  const sorted = useMemo(() => sortChatMessages(messages), [messages]);
  // S-154: while the newest message is the user's (no agent reply yet),
  // the agent's turn is still in flight — lock the composer and show the
  // "Combobulating…" indicator. S-262: the turn stays pending through
  // interim progress replies too (agentTurnPending), so the indicator
  // survives the first chunk and only clears on the final reply. The lock
  // self-expires (CHAT_TURN_LOCK_MS) so a crashed agent can't disable the
  // box forever; the interval re-checks so the box unlocks even without a
  // fresh poll.
  const [generating, setGenerating] = useState(false);
  useEffect(() => {
    const check = () =>
      setGenerating(!stopRequested && agentTurnPending(sorted, Date.now()));
    check();
    const timer = window.setInterval(check, 5000);
    return () => window.clearInterval(timer);
  }, [sorted, stopRequested]);

  // Keep the newest message in view (S-105). S-175: also re-run when the
  // "Combobulating…" row appears/disappears so it is never below the
  // fold, and only while stuck to the bottom (S-175 scroll fix).
  useEffect(() => {
    const el = scrollRef.current;
    if (el && stickToBottom.current) el.scrollTop = el.scrollHeight;
  }, [sorted.length, generating]);

  function trackStickToBottom() {
    const el = scrollRef.current;
    if (!el) return;
    stickToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
  }

  function autoGrow(node: HTMLTextAreaElement | null) {
    if (!node) return;
    node.style.height = "auto";
    node.style.height = `${Math.min(node.scrollHeight, 160)}px`;
  }

  return (
    <div className={openSubagent ? "chat-layout with-side" : "chat-layout"}>
    <div className="chat">
      <div className="chat-scroll" ref={scrollRef} onScroll={trackStickToBottom}>
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
            // S-272: render the ACTUAL sender of each message (from_display is
            // resolved server-side), not the logged-in viewer. Messages from
            // another human — e.g. a platform admin testing someone else's
            // agent — no longer borrow your avatar and "You" label.
            const attribution = chatSenderAttribution(msg, user);
            const isMe = fromUser && attribution.isMe;
            const senderName = fromUser ? attribution.name : agentName;
            // S-265: LLM turn-failure closure gets distinct error styling.
            const turnError = !fromUser && isTurnErrorMessage(msg);
            const toolCalls = fromUser ? [] : chatToolCalls(msg);
            // S-194: image attachments render under the message text.
            const attachments = messageAttachments(msg);
            // S-189: content-derived stable keys for tool calls (no natural ids;
            // list is static message history).
            const callKeys = uniqueContentKeys(toolCalls, (c) => `${msg.id}|${c.name}|${JSON.stringify(c.arguments)}`);
            const renderChatBody = () => {
              if (msg.payload?.message) {
                return <MarkdownMessage text={msg.payload.message} />;
              }
              if (attachments.length > 0) {
                return null;
              }
              return <div className="chat-text">(no text)</div>;
            };
            return (
              <div key={msg.id} className={`chat-row ${fromUser ? "mine" : "theirs"}`}>
                <div className={`chat-avatar ${fromUser ? (isMe ? "me" : "them") : "agent"}`} aria-hidden="true">
                  {initials(fromUser ? senderName : agentName)}
                </div>
                <div className={`chat-bubble${turnError ? " chat-error-bubble" : ""}`}>
                  <div className="chat-head">
                    <span className="chat-name">{fromUser ? (isMe ? "You" : senderName) : agentName}</span>
                    <span className="chat-time">{formatRelativeTime(msg.created_at)}</span>
                    {turnError ? (
                      <span className="chat-status mono error">LLM error</span>
                    ) : null}
                    {!fromUser && msg.status ? <span className="chat-status mono">{msg.status}</span> : null}
                    {/* S-175: surface cancelled user turns; other user statuses stay quiet. */}
                    {fromUser && msg.status === "cancelled" ? (
                      <span className="chat-status mono cancelled">cancelled</span>
                    ) : null}
                  </div>
                  {renderChatBody()}
                  <AttachmentThumbs attachments={attachments} />
                  <ChatToolCallList
                    calls={toolCalls}
                    callKeys={callKeys}
                    onShowSubagent={(c) => setOpenSubagent(c)}
                  />
                </div>
              </div>
            );
          })
        )}
        {generating ? (
          <div className="chat-row theirs chat-typing-row" aria-live="polite">
            <div className="chat-avatar agent" aria-hidden="true">
              {initials(agentName)}
            </div>
            <div className="chat-bubble chat-typing-bubble">
              <span className="chat-typing-dots" aria-hidden="true">
                <span />
                <span />
                <span />
              </span>
              <span className="chat-typing-label">Combobulating…</span>
            </div>
          </div>
        ) : null}
        {footer}
      </div>
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
          disabled={generating}
          onChange={(e) => {
            // S-154: start warming a scaled-to-zero agent as soon as the
            // user begins typing, so the pod is up by send time.
            if (draft === "" && e.target.value !== "" && !generating) {
              const now = Date.now();
              if (now - lastWakeAt.current > 60_000) {
                lastWakeAt.current = now;
                apiPost(`/agents/${agentId}/wake`, token, {}).catch(() => undefined);
              }
            }
            setDraft(e.target.value);
            autoGrow(e.target);
          }}
          placeholder={generating ? `Waiting for ${agentName} to finish…` : `Message ${agentName}…`}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              send();
            }
          }}
          onPaste={(e) => {
            // S-200 follow-up: pasting an image from the clipboard stages it
            // exactly like the paperclip picker. Plain-text pastes fall
            // through untouched.
            const images = extractPastedImages(e.clipboardData?.items);
            if (images.length > 0) {
              e.preventDefault();
              void attachFiles(images);
            }
          }}
        />
        {/* S-194: paperclip opens the file picker. S-200 follow-up: this
            hidden input was missing from the chat page (only the task page
            had one), so fileInputRef was always null and the paperclip
            click did nothing. */}
        <input
          ref={fileInputRef}
          type="file"
          multiple
          accept="image/png,image/jpeg,image/gif,image/webp"
          style={{ display: "none" }}
          aria-hidden="true"
          tabIndex={-1}
          onChange={(e) => {
            const files = e.target.files;
            if (files && files.length > 0) void attachFiles(files);
            // Reset so re-picking the same file fires onChange again.
            e.target.value = "";
          }}
        />
        <button
          type="button"
          className="chat-attach-btn"
          disabled={uploadBusy || generating || pendingAttachments.length >= MAX_MESSAGE_ATTACHMENTS}
          onClick={() => fileInputRef.current?.click()}
          aria-label="Attach image"
          title={uploadBusy ? "Uploading…" : "Attach image (or paste)"}
        >
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
            <path d="M10.5 4.5L5 10a2.1 2.1 0 003 3l5.6-5.6a3.6 3.6 0 10-5.1-5.1L2.8 8a5.1 5.1 0 007.2 7.2l4-4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
        </button>
        {generating ? (
          /* S-175: while a turn is in flight the send button becomes a
             STOP button — cancels the active chat turn server-side. */
          <button
            type="button"
            className="chat-send stop"
            disabled={cancelBusy}
            onClick={() => {
              void cancelTurn();
            }}
            aria-label="Stop agent turn"
            title="Stop — cancel this turn"
          >
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
              <rect x="4" y="4" width="8" height="8" rx="1.5" fill="currentColor" />
            </svg>
          </button>
        ) : (
          <button
            type="submit"
            className="chat-send"
            disabled={busy || (draft.trim() === "" && pendingAttachments.length === 0)}
            aria-label="Send message"
            title="Send"
          >
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
              <path d="M2 14L14 2M14 2H5M14 2V11" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </button>
        )}
      </form>
      {/* S-194: staged attachments shown as removable chips until sent. */}
      {pendingAttachments.length > 0 ? (
        <div className="composer-attachments">
          {pendingAttachments.map((att) => (
            <span key={att.id} className="attachment-chip">
              {/* eslint-disable-next-line @next/next/no-img-element */}
              <img src={att.url} alt={att.filename} />
              <span className="attachment-chip-name">{att.filename}</span>
              <button
                type="button"
                className="attachment-chip-remove"
                aria-label={`Remove ${att.filename}`}
                onClick={() => setPendingAttachments((prev) => withoutAttachment(prev, att.id))}
              >
                ×
              </button>
            </span>
          ))}
        </div>
      ) : null}
      {/* S-178: which LLM this agent talks through, and the per-agent
          thinking level (defaults to the median, Medium). Saved
          immediately via PATCH /agents/{id}. */}
      <div className="composer-meta">
        <span className="composer-model" title="LLM this agent uses">
          🧠 {llmLabel}
        </span>
        <label className="composer-thinking">
          <span>Thinking</span>
          <select
            value={thinkingLevel}
            disabled={thinkingBusy}
            aria-label="Thinking level"
            onChange={(e) => {
              onThinkingLevelChange(e.target.value as ThinkingLevel).catch(
                () => setError("could not save thinking level"),
              );
            }}
          >
            {THINKING_LEVELS.map((l) => (
              <option key={l} value={l}>
                {thinkingLevelLabel(l)}
              </option>
            ))}
          </select>
        </label>
      </div>
      <div className="chat-hint">Enter to send · Shift+Enter for a new line</div>
    </div>
    {openSubagent ? (
      <SubagentThreadPanel info={openSubagent} onClose={() => setOpenSubagent(null)} />
    ) : null}
    </div>
  );

  async function send() {
    // S-194: an image with no text is still a message (the runtime
    // synthesizes the attachment reference for the agent).
    if (draft.trim() === "" && pendingAttachments.length === 0) return;
    setBusy(true);
    setError("");
    // S-175: a fresh send re-arms the pending-turn indicator and forces
    // the thread to scroll to the new message.
    setStopRequested(false);
    stickToBottom.current = true;
    try {
      await apiPost(`/agents/${agentId}/chat`, token, {
        message: draft.trim(),
        // S-194: bind the staged uploads to this message by id.
        attachments: pendingAttachments.map((a) => a.id),
      });
      setDraft("");
      setPendingAttachments([]);
      onSent();
    } catch (err) {
      setError(err instanceof Error ? err.message : "send failed");
    } finally {
      setBusy(false);
    }
  }

  // S-194: validate + upload picked/pasted images, staging them for the
  // next send. Per-file errors surface in the composer error line; the
  // server re-validates content by sniffing bytes regardless.
  async function attachFiles(files: File[] | FileList) {
    const room = MAX_MESSAGE_ATTACHMENTS - pendingAttachments.length;
    if (room <= 0) {
      setError(`At most ${MAX_MESSAGE_ATTACHMENTS} images per message.`);
      return;
    }
    setUploadBusy(true);
    setError("");
    for (const file of Array.from(files).slice(0, room)) {
      const invalid = validateImageFile(file);
      if (invalid) {
        setError(invalid);
        continue;
      }
      try {
        const up = await apiUploadImage("/uploads", token, file, `?squad_id=${encodeURIComponent(squadId)}`);
        setPendingAttachments((prev) => (prev.some((a) => a.id === up.id) ? prev : [...prev, up]));
      } catch (err) {
        setError(err instanceof Error ? err.message : "upload failed");
      }
    }
    setUploadBusy(false);
  }

  // S-175: stop the in-flight agent turn. The control plane marks the
  // newest live user message cancelled; the runtime notices between LLM
  // steps and stops replying. Locally we drop the pending indicator right
  // away so the composer unlocks without waiting for the poll.
  async function cancelTurn() {
    setCancelBusy(true);
    setError("");
    try {
      await apiPost(`/agents/${agentId}/chat/cancel`, token, {});
      setStopRequested(true);
      onSent();
    } catch (err) {
      setError(err instanceof Error ? err.message : "cancel failed");
    } finally {
      setCancelBusy(false);
    }
  }
}

// WP7 (S-112) — LLM model binding fields (ADR-0010 D4): primary is
// required, fallback optional. Options come from the caller's granted+
// active models (/models/me); the selected primary is excluded from the
// fallback list. Warnings are soft — never blocking.
//
// S-169: converted to a controlled component. The Configuration tab owns
// the values and the single Save; this renders selects + soft warnings
// only (the old "Save binding" button folded into "Save configuration").
function LlmBindingFields({
  primary,
  fallback,
  onPrimaryChange,
  onFallbackChange,
}: {
  readonly primary: string;
  readonly fallback: string;
  readonly onPrimaryChange: (id: string) => void;
  readonly onFallbackChange: (id: string) => void;
}) {
  const myModels = useApi<AIModel[]>("/models/me", 60000);

  const models = myModels.data ?? [];
  const primaryOpts = withCurrentOption(selectableModels(models), models, primary);
  const fallbackOpts = fallbackChoices(models, primary);
  const primaryModel = findModelById(models, primary);
  const fallbackModel = findModelById(models, fallback);
  const warnings = bindingWarnings(primaryModel, fallbackModel);

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
              onPrimaryChange(e.target.value);
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
              onFallbackChange(e.target.value);
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

      <span className="field-hint">
        Saving the configuration re-provisions the agent&apos;s gateway key immediately.
      </span>
    </div>
  );
}

// S-189/S2004: helper keeps the attachment-chip remove handler under
// the function-nesting depth limit.
function withoutAttachment(list: UploadRef[], id: string): UploadRef[] {
  return list.filter((a) => a.id !== id);
}

function GrantModal({
  existing,
  agentId,
  token,
  onClose,
  onGranted,
}: {
  readonly existing: AgentPermission[];
  readonly agentId: string;
  readonly token: string;
  readonly onClose: () => void;
  readonly onGranted: () => void;
}) {
  const [typeIdx, setTypeIdx] = useState(0);
  const [resourceId, setResourceId] = useState("");
  // TG-4: optional grant-level constraints for rest (BYO) resources.
  const [methods, setMethods] = useState<string[]>([]);
  const [pathAllow, setPathAllow] = useState("");
  const [ratePerMin, setRatePerMin] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const selectedType = GRANTABLE_TYPES[typeIdx];
  const resources = useApi<RegistryResource[]>(`/registry/${selectedType.path}`, 0);
  const selectedResource = (resources.data || []).find((r) => r.id === resourceId);
  const ceiling = (selectedResource?.policy_ceiling ?? undefined) as RestCeiling | undefined;
  const isRest = selectedType.type === "api";

  function toggleGrantMethod(m: string) {
    setMethods((prev) => (prev.includes(m) ? prev.filter((x) => x !== m) : [...prev, m]));
  }

  const draftConstraints: RestCeiling = {};
  if (isRest && methods.length > 0) draftConstraints.methods = methods;
  if (isRest && splitList(pathAllow).length > 0) draftConstraints.path_allow = splitList(pathAllow);
  if (isRest && ratePerMin.trim() !== "" && Number(ratePerMin) > 0) draftConstraints.rate_per_min = Number(ratePerMin);
  const hasDraft = Object.keys(draftConstraints).length > 0;

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
              // Preserve existing grants' constraints (TG-2) — the PUT
              // replaces the whole set, so drop nothing silently.
              ...existing.map((p) => ({
                resource_type: p.resource_type,
                resource_id: p.resource_id,
                ...(p.constraints && Object.keys(p.constraints as object).length > 0 ? { constraints: p.constraints } : {}),
              })),
              { resource_type: selectedType.type, resource_id: resourceId, ...(hasDraft ? { constraints: draftConstraints } : {}) },
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
        {isRest && selectedResource ? (
          <>
            <div className="notice">Resource ceiling: {restCeilingSummary(ceiling)}</div>
            <div className="field">
              <span>Narrow methods (optional — must stay within the ceiling)</span>
              <div className="field-row">
                {(ceiling?.methods ?? []).map((m) => (
                  <label key={m} className="field-checkbox">
                    <input type="checkbox" checked={methods.includes(m)} onChange={() => toggleGrantMethod(m)} />
                    {m}
                  </label>
                ))}
              </div>
              {(ceiling?.methods ?? []).length === 0 ? (
                <span className="field-hint">Ceiling allows no methods — this resource cannot be usefully granted.</span>
              ) : null}
            </div>
            <div className="field-row">
              <label className="field">
                <span>Narrow path allow (optional)</span>
                <input value={pathAllow} onChange={(e) => setPathAllow(e.target.value)} placeholder="/issues/**" />
              </label>
              <label className="field">
                <span>Rate / min (optional)</span>
                <input value={ratePerMin} onChange={(e) => setRatePerMin(e.target.value)} placeholder="30" />
              </label>
            </div>
            {hasDraft ? (
              <div className="notice">Effective for this agent: {restCeilingSummary(foldRestConstraints(ceiling, draftConstraints))}</div>
            ) : (
              <div className="notice">No grant narrowing — agent gets the full resource ceiling.</div>
            )}
          </>
        ) : null}
      </ModalForm>
    </Modal>
  );
}

// S-189/S4323: shared shape for the two metering fetch states.
type MeteringState = { loading: boolean; error: string | null; data: MeteringSummary | null };

// S-189/S3776: title-row metric chips and the section tab bar extracted
// from AgentProfilePage; render output identical to the inline JSX.
function AgentMetricChips({
  live,
  stalled,
  tasks,
  resourceGrants,
  metering,
  meteringMtd,
  agent,
  contextTokens,
}: {
  readonly live: Task[];
  readonly stalled: Task[];
  readonly tasks: Task[];
  readonly resourceGrants: AgentPermission[];
  readonly metering: MeteringState;
  readonly meteringMtd: MeteringState;
  readonly agent: Agent | undefined;
  readonly contextTokens: number | null;
}) {
  return (
    <div className="metric-chips agent-title-metrics">
      <span className={stalled.length > 0 ? "metric-chip attention" : "metric-chip"}>
        <span className="metric-chip-label">Current lease</span>
        <span className="metric-chip-value">{live.length > 0 ? "1 task" : "none"}</span>
        <span className="metric-chip-sub">{leaseSub(live, stalled)}</span>
      </span>
      <span className="metric-chip">
        <span className="metric-chip-label">MTD spend</span>
        <span className="metric-chip-value">{meteringMtd.loading ? "…" : formatCost(meteringMtd.data)}</span>
        <span className="metric-chip-sub">
          {meteringMtd.error ? "owner and platform admins only" : `lifetime ${formatCost(metering.data)}`}
        </span>
      </span>
      <span className="metric-chip">
        <span className="metric-chip-label">Tasks</span>
        <span className="metric-chip-value">{tasks.length}</span>
        <span className="metric-chip-sub">{live.length} running now</span>
      </span>
      <span className="metric-chip">
        <span className="metric-chip-label">Grants</span>
        <span className="metric-chip-value">{resourceGrants.length}</span>
        <span className="metric-chip-sub">{resourceGrants.length === 0 ? "none" : "resource grants"}</span>
      </span>
      <span className="metric-chip">
        <span className="metric-chip-label">Workspace</span>
        <span className="metric-chip-value">{storageDisplay(agent?.storage_enabled, agent?.storage_size)}</span>
        <span className="metric-chip-sub">{agent?.storage_enabled ? "durable" : "ephemeral"}</span>
      </span>
      <span className="metric-chip">
        <span className="metric-chip-label">Context</span>
        <span className="metric-chip-value">{contextTokens === null ? "—" : formatContextTokens(contextTokens)}</span>
        <span className="metric-chip-sub">tokens · last agent turn</span>
      </span>
    </div>
  );
}

function AgentSectionTabs({
  tab,
  onSelect,
}: {
  readonly tab: "chat" | "config" | "inbox";
  readonly onSelect: (tab: "chat" | "config" | "inbox") => void;
}) {
  return (
    <nav className="squad-tabs agent-tabs" aria-label="Agent sections" style={{ marginTop: "var(--space-4)" }}>
      <button
        type="button"
        className={tab === "chat" ? "squad-tab active" : "squad-tab"}
        aria-current={tab === "chat" ? "page" : undefined}
        onClick={() => onSelect("chat")}
      >
        Chat
      </button>
      <button
        type="button"
        className={tab === "config" ? "squad-tab active" : "squad-tab"}
        aria-current={tab === "config" ? "page" : undefined}
        onClick={() => onSelect("config")}
      >
        Configuration
      </button>
      <button
        type="button"
        className={tab === "inbox" ? "squad-tab active" : "squad-tab"}
        aria-current={tab === "inbox" ? "page" : undefined}
        onClick={() => onSelect("inbox")}
      >
        Message delivery
      </button>
    </nav>
  );
}

// S-189/S3776: durable-storage field extracted from AgentConfigPane;
// render output identical to the inline JSX.
function StorageWorkspaceField({
  storageEnabled,
  storageSize,
  storageInvalid,
  onChangeEnabled,
  onChangeSize,
}: {
  readonly storageEnabled: boolean;
  readonly storageSize: string;
  readonly storageInvalid: boolean;
  readonly onChangeEnabled: (v: boolean) => void;
  readonly onChangeSize: (v: string) => void;
}) {
  return (
    <div className="field">
      <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", cursor: "pointer" }}>
        <input
          type="checkbox"
          checked={storageEnabled}
          onChange={(e) => onChangeEnabled(e.target.checked)}
        />
        <span>Durable workspace storage</span>
      </label>
      {storageEnabled ? (
        <>
          <div style={{ display: "flex", gap: "0.4rem", marginTop: "0.5rem", flexWrap: "wrap" }}>
            {STORAGE_PRESETS.map((preset) => (
              <button
                key={preset}
                type="button"
                className={`btn btn-sm${storageSize === preset ? " btn-primary" : ""}`}
                onClick={() => onChangeSize(preset)}
              >
                {preset}
              </button>
            ))}
          </div>
          <input
            value={storageSize}
            onChange={(e) => onChangeSize(e.target.value)}
            placeholder="custom, e.g. 3Gi"
            style={{ marginTop: "0.5rem" }}
            aria-label="Storage size"
          />
          {storageInvalid ? (
            <p className="field-hint" style={{ color: "var(--danger, #c0392b)" }}>
              Must be a positive quantity like 1Gi, 2Gi or 500M.
            </p>
          ) : null}
        </>
      ) : null}
      <p className="field-hint">
        Durable workspace storage that survives restarts. The platform caps the maximum size;
        the storage class is managed by your platform admin.
      </p>
    </div>
  );
}

// S-189/S3776: the two tool-call render branches extracted from
// ChatToolCallList; render output identical to the inline JSX.
function SubagentCallChip({
  call,
  subagent,
  onShowSubagent,
}: {
  readonly call: ChatToolCall;
  readonly subagent: SubagentInfo;
  readonly onShowSubagent: (c: SubagentInfo | null) => void;
}) {
  return (
    <div className={`chat-tool subagent-chip${call.ok ? "" : " failed"}`}>
      <div className="chat-tool-summary">
        <span className="chat-tool-name">🤖 subagent</span>
        <span className="chat-tool-args mono">{subagentSummary(subagent)}</span>
        <span className="chat-tool-state">{call.ok ? "ok" : "failed"}</span>
        <button
          type="button"
          className="btn ghost small"
          onClick={() => onShowSubagent(subagent)}
        >
          Details
        </button>
      </div>
      {call.result ? (
        <div className="chat-subagent-final">{truncateText(call.result, 400)}</div>
      ) : null}
    </div>
  );
}

function PlainToolCall({ call }: { readonly call: ChatToolCall }) {
  const argsSummary = summarizeToolArgs(call.arguments);
  return (
    <details className={`chat-tool${call.ok ? "" : " failed"}`}>
      <summary className="chat-tool-summary">
        <span className="chat-tool-name">🔧 {call.name}</span>
        {argsSummary ? <span className="chat-tool-args mono">{argsSummary}</span> : null}
        <span className="chat-tool-state">{call.ok ? "ok" : "failed"}</span>
      </summary>
      <pre className="chat-tool-detail mono">{prettyToolArgs(call.arguments)}</pre>
      {call.result ? (
        <pre className="chat-tool-detail mono result">{truncateText(call.result, 4000)}</pre>
      ) : null}
    </details>
  );
}

function ChatToolCallList({
  calls,
  callKeys,
  onShowSubagent,
}: {
  readonly calls: ChatToolCall[];
  readonly callKeys: string[];
  readonly onShowSubagent: (c: SubagentInfo | null) => void;
}) {
  if (calls.length === 0) {
    return null;
  }
  return (
    <div className="chat-tools">
      {calls.map((call, idx) =>
        call.name === "spawn_subagent" && call.subagent ? (
          <SubagentCallChip key={`sub-${callKeys[idx]}`} call={call} subagent={call.subagent} onShowSubagent={onShowSubagent} />
        ) : (
          <PlainToolCall key={callKeys[idx]} call={call} />
        ),
      )}
    </div>
  );
}
