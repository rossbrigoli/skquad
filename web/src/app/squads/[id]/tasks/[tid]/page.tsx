"use client";

import Link from "next/link";
import { useRef, useState } from "react";
import { useParams, useRouter } from "next/navigation";
import { ActivityFeed } from "../../../../../components/ActivityFeed";
import { AuthGate } from "../../../../../components/AuthGate";
import { ConfirmDialog } from "../../../../../components/ConfirmDialog";
import { Modal, ModalForm } from "../../../../../components/Modal";
import { AppShell } from "../../../../../components/AppShell";
import { AttachmentThumbs } from "../../../../../components/AttachmentThumbs";
import { Collapsible } from "../../../../../components/Collapsible";
import { EmptyState } from "../../../../../components/EmptyState";
import { StatusChip } from "../../../../../components/StatusChip";
import { useApi } from "../../../../../lib/useApi";
import { useAuth } from "../../../../../lib/auth";
import { apiDelete, apiPatch, apiPost, apiUploadImage, type Agent, type AuditEntry, type Message, type Task } from "../../../../../lib/api";
import { messageAttachments, validateImageFile } from "../../../../../lib/uploads";
import { formatRelativeTime, leaseState, messageText } from "../../../../../lib/format";
import { threadActorLabel, timelineActorLabel } from "../../../../../lib/actorDisplay";
import { formatTaskRef } from "../../../../../lib/taskRef";
import { taskResultInfo } from "../../../../../lib/taskResult";
import { taskStatus } from "../../../../../lib/status";

const MOVE_TARGETS: { status: string; label: string }[] = [
  { status: "backlog", label: "Backlog" },
  { status: "todo", label: "To do" },
  { status: "in-progress", label: "In progress" },
  { status: "in-review", label: "In review" },
  { status: "done", label: "Done" },
  { status: "blocked", label: "Blocked" },
];

// S-189/S3776: lease hint extracted so the page component stays under
// the cognitive-complexity limit.
function leaseSuffix(state: string): string {
  if (state === "running") {
    return " · lease live";
  }
  if (state === "stalled") {
    return " · lease stalled";
  }
  return "";
}

export default function TaskDetailPage() {
  const params = useParams<{ id: string; tid: string }>();
  const squadId = String(params?.id ?? "");
  const taskId = String(params?.tid ?? "");
  const router = useRouter();
  const { token, authed } = useAuth();

  const task = useApi<Task>(`/tasks/${taskId}`, 15000);
  const thread = useApi<Message[]>(`/tasks/${taskId}/messages`, 15000);
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 60000);
  const audit = useApi<AuditEntry[]>(`/squads/${squadId}/audit?limit=50`, 30000);

  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  // S-194: attach a screenshot to the task thread (uploaded, then
  // posted as a task message carrying the attachment reference).
  const [attachBusy, setAttachBusy] = useState(false);
  const attachInputRef = useRef<HTMLInputElement | null>(null);

  const current = task.data;
  const assignee = (agents.data || []).find((a) => a.id === current?.assignee_agent_id);
  const messages = thread.data || [];
  const timeline = (audit.data || []).filter((entry) => entry.resource_id === taskId);
  const resultInfo = current ? taskResultInfo(current, messages) : null;

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

  // S-194: upload a picked image into this task's squad and post it to
  // the task thread. The control plane binds the attachment to the
  // message and delivers the reference to the assigned agent.
  const attachImage = async (file: File) => {
    if (!authed || !current) {
      return;
    }
    const invalid = validateImageFile(file);
    if (invalid) {
      setActionError(invalid);
      return;
    }
    setAttachBusy(true);
    setActionError("");
    try {
      const up = await apiUploadImage("/uploads", token, file, `?squad_id=${encodeURIComponent(squadId)}`);
      await apiPost(`/tasks/${taskId}/messages`, token, {
        message: `📎 Screenshot attached: ${up.filename}`,
        attachments: [up.id],
      });
      thread.refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "attach failed");
    } finally {
      setAttachBusy(false);
    }
  };

  if (task.error) {
    return (
      <AuthGate>
        <AppShell>
          <EmptyState title="Task not found" hint={task.error} />
        </AppShell>
      </AuthGate>
    );
  }

  if (!current) {
    return (
      <AuthGate>
        <AppShell>
          <EmptyState title="Loading task…" hint="Fetching task, thread and history." />
        </AppShell>
      </AuthGate>
    );
  }

  return (
    <AuthGate>
      <AppShell>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "var(--space-3)" }}>
          <h1 className="page-title">
            {formatTaskRef(current) ? (
              <span className="task-ref" title={`Task reference ${formatTaskRef(current)}`}>
                {formatTaskRef(current)}
              </span>
            ) : null}
            {current.title}
          </h1>
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
        {/* S-235: back-to-board moved to the top, just under the title, so
            it's reachable without scrolling to the bottom of a long page. */}
        <p style={{ margin: "var(--space-2) 0 0" }}>
          <Link href={`/squads/${squadId}/board`} style={{ color: "var(--accent)" }}>
            ← Back to board
          </Link>
        </p>
        {/* S-235: lease/assignee meta row doubles as the home for the
            agent-chat button (was buried at the bottom of the thread). */}
        <div
          style={{
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
            gap: "var(--space-3)",
            marginTop: "var(--space-1)",
          }}
        >
          <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)", margin: 0 }}>
            {assignee ? (
              <>
                assigned to <strong>{assignee.name}</strong>
              </>
            ) : (
              "unassigned"
            )}{" "}
            · updated {formatRelativeTime(current.updated_at)}
            {leaseSuffix(leaseState(current))}
          </p>
          {assignee ? (
            <Link
              href={`/squads/${squadId}/agents/${assignee.id}`}
              className="btn btn-sm btn-primary"
            >
              Talk to {assignee.name}
            </Link>
          ) : null}
        </div>
        {current.description ? (
          <p style={{ whiteSpace: "pre-wrap", marginTop: "var(--space-3)" }}>{current.description}</p>
        ) : null}

        {resultInfo ? (
          <section className={`result-panel ${resultInfo.tone}`} aria-label="Task result">
            <div className="result-head">
              <span className={`chip ${resultInfo.tone === "blocked" ? "chip-blocked" : "chip-done"}`}>
                {resultInfo.label}
              </span>
              {resultInfo.at ? <span className="muted">{formatRelativeTime(resultInfo.at)}</span> : null}
            </div>
            <p className="result-text">{resultInfo.text}</p>
          </section>
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

        <TaskThreadSection
          current={current}
          messages={messages}
          attachBusy={attachBusy}
          attachInputRef={attachInputRef}
          onAttach={(file) => void attachImage(file)}
        />

        {/* S-235: status timeline is collapsible (expanded by default;
            session-persisted via the shared Collapsible pattern). */}
        <Collapsible id={`task-timeline-${taskId}`} initialOpen title="Status timeline">
          <ActivityFeed
            squadId={squadId}
            entries={timeline}
            nameFor={(entry) => timelineActorLabel(entry)}
            emptyTitle="No recorded changes yet"
            emptyHint="Status moves, assignments and messages on this task appear here."
          />
        </Collapsible>

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
  readonly task: Task;
  readonly token: string;
  readonly onCancel: () => void;
  readonly onSaved: () => void;
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

// S-189/S3776: thread section extracted from TaskDetailPage so the page
// component stays under the cognitive-complexity ceiling.
// S-235: the section is collapsible (expanded by default, session
// persisted). The attach control moved into the body because the
// Collapsible header is itself a toggle button (no nested buttons).
function TaskThreadSection({
  current,
  messages,
  attachBusy,
  attachInputRef,
  onAttach,
}: {
  readonly current: Task;
  readonly messages: Message[];
  readonly attachBusy: boolean;
  readonly attachInputRef: React.RefObject<HTMLInputElement | null>;
  readonly onAttach: (file: File) => void;
}) {
  return (
    <Collapsible
      id={`task-thread-${current.id}`}
      initialOpen
      title={
        <span style={{ display: "flex", alignItems: "center", gap: "var(--space-2)" }}>
          Thread <span className="chip chip-idle">{messages.length}</span>
        </span>
      }
    >
      {/* S-194: attach a defect screenshot straight to the thread. */}
      {current.assignee_agent_id ? (
        <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: "var(--space-3)" }}>
          <input
            ref={attachInputRef}
            type="file"
            accept="image/png,image/jpeg,image/gif,image/webp"
            style={{ display: "none" }}
            onChange={(e) => {
              const file = e.target.files?.[0];
              e.target.value = "";
              if (file) onAttach(file);
            }}
          />
          <button
            type="button"
            className="btn btn-sm"
            disabled={attachBusy}
            onClick={() => attachInputRef.current?.click()}
            title={attachBusy ? "Uploading…" : "Attach an image (png/jpg/gif/webp, ≤5 MB)"}
          >
            {attachBusy ? "Uploading…" : "📎 Attach image"}
          </button>
        </div>
      ) : null}
      {messages.length === 0 ? (
        <EmptyState
          title={current.assignee_agent_id ? "No task messages yet" : "Assign an agent to start a thread"}
          hint="Messages here are scoped to this task and delivered to the assigned agent."
        />
      ) : (
        <div className="entity-list">
          {messages.map((message) => (
            <TaskMessageRow key={message.id} message={message} />
          ))}
        </div>
      )}
    </Collapsible>
  );
}

// TaskMessageRow renders one thread message with its attachments.
function TaskMessageRow({ message }: { readonly message: Message }) {
  const statusNote =
    message.status !== "delivered" && message.status !== "acked" ? ` · ${message.status}` : "";
  return (
    <div className="entity-row">
      <div className="entity-main">
        <span className="entity-title">{messageText(message)}</span>
        <span className="entity-meta">
          {threadActorLabel(message)} ·{" "}
          {formatRelativeTime(message.created_at)}
          {statusNote}
        </span>
        {/* S-194: screenshots carried by this thread message. */}
        <AttachmentThumbs attachments={messageAttachments(message)} />
      </div>
    </div>
  );
}
