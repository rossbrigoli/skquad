"use client";

import Link from "next/link";
import { useCallback, useState } from "react";
import { useParams, useRouter } from "next/navigation";
import { ActivityFeed } from "../../../../../components/ActivityFeed";
import { AuthGate } from "../../../../../components/AuthGate";
import { ConfirmDialog } from "../../../../../components/ConfirmDialog";
import { Modal, ModalForm } from "../../../../../components/Modal";
import { AppShell } from "../../../../../components/AppShell";
import { EmptyState } from "../../../../../components/EmptyState";
import { SquadRail } from "../../../../../components/SquadRail";
import { StatusChip } from "../../../../../components/StatusChip";
import { useApi } from "../../../../../lib/useApi";
import { useAuth } from "../../../../../lib/auth";
import { apiDelete, apiPatch, apiPost, type Agent, type AuditEntry, type Message, type Squad, type Task } from "../../../../../lib/api";
import { formatRelativeTime, leaseState, messageText } from "../../../../../lib/format";
import { taskStatus } from "../../../../../lib/status";

const MOVE_TARGETS: { status: string; label: string }[] = [
  { status: "todo", label: "To do" },
  { status: "in-progress", label: "In progress" },
  { status: "in-review", label: "In review" },
  { status: "done", label: "Done" },
  { status: "blocked", label: "Blocked" },
];

export default function TaskDetailPage() {
  const params = useParams<{ id: string; tid: string }>();
  const squadId = String(params?.id ?? "");
  const taskId = String(params?.tid ?? "");
  const router = useRouter();
  const { token, authed } = useAuth();

  const task = useApi<Task>(`/tasks/${taskId}`, 15000);
  const thread = useApi<Message[]>(`/tasks/${taskId}/messages`, 15000);
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 60000);
  const squads = useApi<Squad[]>("/squads");
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=50`, 30000);

  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const current = task.data;
  const assignee = (agents.data || []).find((a) => a.id === current?.assignee_agent_id);
  const squad = (squads.data || []).find((s) => s.id === squadId);
  const messages = thread.data || [];
  const timeline = (audit.data || []).filter((entry) => entry.resource_id === taskId);

  const send = useCallback(async () => {
    if (!authed || !draft.trim() || !current) {
      return;
    }
    setSending(true);
    setActionError("");
    try {
      await apiPost(`/tasks/${taskId}/messages`, token, { message: draft.trim() });
      setDraft("");
      thread.refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "send failed");
    } finally {
      setSending(false);
    }
  }, [token, authed, draft, current, taskId, thread]);

  const move = async (status: string) => {
    if (!authed || !current) {
      return;
    }
    setBusy(true);
    setActionError("");
    try {
      await apiPost(`/tasks/${taskId}/move`, token, { status });
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "move failed");
    } finally {
      setBusy(false);
    }
  };

  const reassign = async (agentId: string) => {
    if (!authed) {
      return;
    }
    setBusy(true);
    setActionError("");
    try {
      await apiPatch(`/tasks/${taskId}`, token, { assignee_agent_id: agentId });
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "reassign failed");
    } finally {
      setBusy(false);
    }
  };

  if (task.error) {
    return (
      <AuthGate>
        <AppShell secondary={<SquadRail squadId={squadId} squadName={squad?.name || "Squad"} />}>
          <EmptyState title="Task not found" hint={task.error} />
        </AppShell>
      </AuthGate>
    );
  }

  if (!current) {
    return (
      <AuthGate>
        <AppShell secondary={<SquadRail squadId={squadId} squadName={squad?.name || "Squad"} />}>
          <EmptyState title="Loading task…" hint="Fetching task, thread and history." />
        </AppShell>
      </AuthGate>
    );
  }

  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName={squad?.name || "…"} />}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "var(--space-3)" }}>
          <h1 className="page-title">{current.title}</h1>
          <div style={{ display: "flex", gap: "var(--space-2)", alignItems: "center" }}>
            <StatusChip status={taskStatus(current)} />
            <button type="button" className="btn btn-sm" onClick={() => setEditing(true)}>
              Edit
            </button>
            <button type="button" className="btn btn-sm btn-danger" onClick={() => setDeleting(true)}>
              Delete
            </button>
          </div>
        </div>
        <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)" }}>
          {assignee ? (
            <>
              assigned to <strong>{assignee.name}</strong>
            </>
          ) : (
            "unassigned"
          )}{" "}
          · updated {formatRelativeTime(current.updated_at)}
          {leaseState(current) === "running" ? " · lease live" : ""}
          {leaseState(current) === "stalled" ? " · lease stalled" : ""}
        </p>
        {current.description ? (
          <p style={{ whiteSpace: "pre-wrap", marginTop: "var(--space-3)" }}>{current.description}</p>
        ) : null}

        {actionError ? <div className="notice error">{actionError}</div> : null}

        <section style={{ marginTop: "var(--space-4)" }}>
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-2)" }}>Actions</h2>
          <div style={{ display: "flex", flexWrap: "wrap", gap: "var(--space-2)", alignItems: "center" }}>
            {MOVE_TARGETS.filter((t) => t.status !== current.status).map((t) => (
              <button key={t.status} type="button" className="btn" disabled={busy} onClick={() => void move(t.status)}>
                Move to {t.label}
              </button>
            ))}
            <label style={{ display: "flex", alignItems: "center", gap: "var(--space-2)" }}>
              <span className="muted">Reassign:</span>
              <select
                className="btn"
                value={current.assignee_agent_id ?? ""}
                disabled={busy}
                onChange={(e) => void reassign(e.target.value)}
              >
                <option value="" disabled>
                  select agent…
                </option>
                {(agents.data || []).map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>
            Thread <span className="chip chip-idle">{messages.length}</span>
          </h2>
          {messages.length === 0 ? (
            <EmptyState
              title={current.assignee_agent_id ? "No task messages yet" : "Assign an agent to start a thread"}
              hint="Messages here are scoped to this task and delivered to the assigned agent."
            />
          ) : (
            <div className="entity-list">
              {messages.map((message) => (
                <div key={message.id} className="entity-row">
                  <div className="entity-main">
                    <span className="entity-title">{messageText(message)}</span>
                    <span className="entity-meta">
                      {message.from_type === "user" ? "you" : `agent ${message.from_id.slice(0, 8)}`} ·{" "}
                      {formatRelativeTime(message.created_at)}
                      {message.status !== "delivered" && message.status !== "acked" ? ` · ${message.status}` : ""}
                    </span>
                  </div>
                </div>
              ))}
            </div>
          )}
          {current.assignee_agent_id ? (
            <div style={{ display: "flex", gap: "var(--space-2)", marginTop: "var(--space-3)" }}>
              <input
                className="input"
                style={{ flex: 1 }}
                placeholder={`Message ${assignee?.name || "the assignee"} about this task…`}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault();
                    send().catch(() => undefined);
                  }
                }}
              />
              <button type="button" className="btn btn-primary" disabled={sending || !draft.trim()} onClick={() => send().catch(() => undefined)}>
                {sending ? "Sending…" : "Send"}
              </button>
            </div>
          ) : null}
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>Status timeline</h2>
          <ActivityFeed
            squadId={squadId}
            entries={timeline}
            emptyTitle="No recorded changes yet"
            emptyHint="Status moves, assignments and messages on this task appear here."
          />
        </section>

        <p style={{ marginTop: "var(--space-5)" }}>
          <Link href={`/squads/${squadId}/board`} style={{ color: "var(--accent)" }}>
            ← Back to board
          </Link>
        </p>

        {editing ? (
          <Modal title="Edit task" onClose={() => setEditing(false)}>
            <TaskEditForm
              task={current}
              token={token}
              onCancel={() => setEditing(false)}
              onSaved={() => {
                setEditing(false);
                task.refresh();
              }}
            />
          </Modal>
        ) : null}

        {deleting ? (
          <ConfirmDialog
            title="Delete this task?"
            body={`“${current.title}” and its full message history will be removed. This cannot be undone.`}
            onConfirm={async () => {
              await apiDelete(`/tasks/${taskId}`, token);
              router.push(`/squads/${squadId}/board`);
            }}
            onClose={() => setDeleting(false)}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

function TaskEditForm({
  task,
  token,
  onCancel,
  onSaved,
}: {
  task: Task;
  token: string;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const [title, setTitle] = useState(task.title ?? "");
  const [description, setDescription] = useState(task.description ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <ModalForm
      busy={busy}
      error={error}
      submitLabel="Save task"
      submitDisabled={title.trim() === ""}
      onCancel={onCancel}
      onSubmit={async () => {
        setBusy(true);
        setError("");
        try {
          await apiPatch<Task>(`/tasks/${task.id}`, token, { title: title.trim(), description });
          onSaved();
        } catch (err) {
          setError(err instanceof Error ? err.message : "update failed");
          setBusy(false);
        }
      }}
    >
      <label className="field">
        <span>Title</span>
        <input value={title} onChange={(e) => setTitle(e.target.value)} autoFocus />
      </label>
      <label className="field">
        <span>Description</span>
        <textarea value={description} onChange={(e) => setDescription(e.target.value)} />
      </label>
    </ModalForm>
  );
}
