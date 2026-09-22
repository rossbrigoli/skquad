"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { useState, type DragEvent } from "react";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { EmptyState } from "../../../../components/EmptyState";
import { Modal, ModalForm } from "../../../../components/Modal";
import { SquadRail } from "../../../../components/SquadRail";
import { StatusChip } from "../../../../components/StatusChip";
import { useApi } from "../../../../lib/useApi";
import { useAuth } from "../../../../lib/auth";
import { apiPatch, apiPost, type Agent, type BoardPayload, type Squad, type Task, type TaskStatus } from "../../../../lib/api";
import { formatRelativeTime, leaseState } from "../../../../lib/format";
import { taskStatus } from "../../../../lib/status";
import {
  boardColumnsFromOperatingModel,
  moveColumn,
  visibleColumns,
  type BoardColumnConfig,
} from "../../../../lib/boardColumns";

export default function SquadBoardPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id || "");
  const { token } = useAuth();
  const squads = useApi<Squad[]>("/squads");
  const board = useApi<BoardPayload>(`/squads/${squadId}/board`, 15000);
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 30000);
  const [addingTo, setAddingTo] = useState<TaskStatus | null>(null);
  const [configuring, setConfiguring] = useState(false);
  const [dragOver, setDragOver] = useState<TaskStatus | null>(null);

  const squad = (squads.data || []).find((s) => s.id === squadId);
  const tasks = board.data?.tasks || [];
  const agentItems = agents.data || [];
  const columns = visibleColumns(boardColumnsFromOperatingModel(squad?.operating_model));
  const agentName = (id?: string) =>
    agentItems.find((a) => a.id === id)?.name || (id ? id.slice(0, 8) : "unassigned");

  async function moveTask(taskId: string, status: TaskStatus) {
    await apiPost(`/tasks/${taskId}/move`, token, { status });
    board.refresh();
  }

  function onDrop(event: DragEvent<HTMLDivElement>, status: TaskStatus) {
    event.preventDefault();
    setDragOver(null);
    const taskId = event.dataTransfer.getData("text/skquad-task");
    if (!taskId) return;
    const task = tasks.find((t) => t.id === taskId);
    if (!task || task.status === status) return;
    void moveTask(taskId, status);
  }

  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName={squad?.name || "Squad"} />}>
        <div className="section-head">
          <h1 className="page-title" style={{ margin: 0 }}>
            Board
          </h1>
          <div style={{ display: "flex", gap: "var(--space-2)" }}>
            <button type="button" className="btn btn-sm" onClick={() => setConfiguring(true)} disabled={!squad}>
              Configure columns
            </button>
            <button type="button" className="btn btn-primary btn-sm" onClick={() => setAddingTo("todo")}>
              + Add task
            </button>
          </div>
        </div>
        {board.error ? <div className="notice error">{board.error}</div> : null}
        {!board.loading && tasks.length === 0 ? (
          <EmptyState
            title="The board is empty"
            hint="Add a task and assign it to an agent — the squad wakes up from there."
            action={
              <button type="button" className="btn btn-primary" onClick={() => setAddingTo("todo")}>
                + Add the first task
              </button>
            }
          />
        ) : (
          <div className="board" style={{ gridTemplateColumns: `repeat(${Math.max(columns.length, 1)}, minmax(240px, 1fr))` }}>
            {columns.map((col) => {
              const colTasks = tasks.filter((t) => t.status === col.status);
              return (
                <div
                  key={col.status}
                  className={`board-column${dragOver === col.status ? " drag-over" : ""}`}
                  onDragOver={(e) => {
                    e.preventDefault();
                    setDragOver(col.status);
                  }}
                  onDragLeave={() => setDragOver(null)}
                  onDrop={(e) => onDrop(e, col.status)}
                >
                  <div className="board-col-head">
                    <span>
                      <span className="board-col-title">{col.label}</span>
                      <span className="board-col-count">{colTasks.length}</span>
                    </span>
                    <button
                      type="button"
                      className="btn btn-sm"
                      onClick={() => setAddingTo(col.status)}
                      title={`Add task to ${col.label}`}
                    >
                      +
                    </button>
                  </div>
                  <div className="board-col-body">
                    {colTasks.map((task) => {
                      const lease = leaseState(task);
                      return (
                        <div
                          key={task.id}
                          className="task-card"
                          draggable
                          onDragStart={(e) => {
                            e.dataTransfer.setData("text/skquad-task", task.id);
                            e.dataTransfer.effectAllowed = "move";
                          }}
                        >
                          <Link href={`/squads/${squadId}/tasks/${task.id}`} className="task-title">
                            {task.title}
                          </Link>
                          <div className="task-meta">
                            <span>{agentName(task.assignee_agent_id)}</span>
                            {lease === "running" ? <StatusChip status="running" /> : null}
                            {lease === "stalled" ? <StatusChip status="stalled" /> : null}
                            {task.status === "blocked" ? <StatusChip status="blocked" /> : null}
                            {task.status === "in-review" ? <StatusChip status={taskStatus(task)} /> : null}
                            <span>{formatRelativeTime(task.updated_at)}</span>
                          </div>
                        </div>
                      );
                    })}
                    {colTasks.length === 0 ? (
                      <div style={{ padding: "var(--space-3)", color: "var(--ink-faint)", fontSize: "var(--text-sm)" }}>
                        Drop tasks here
                      </div>
                    ) : null}
                  </div>
                </div>
              );
            })}
          </div>
        )}

        {addingTo ? (
          <TaskCreateModal
            columnLabel={columns.find((c) => c.status === addingTo)?.label || addingTo}
            agents={agentItems}
            squadId={squadId}
            token={token}
            targetStatus={addingTo}
            onClose={() => setAddingTo(null)}
            onCreated={() => {
              setAddingTo(null);
              board.refresh();
            }}
          />
        ) : null}

        {configuring && squad ? (
          <ColumnConfigModal
            squad={squad}
            token={token}
            onClose={() => setConfiguring(false)}
            onSaved={() => {
              setConfiguring(false);
              squads.refresh();
            }}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

function TaskCreateModal({
  columnLabel,
  agents,
  squadId,
  token,
  targetStatus,
  onClose,
  onCreated,
}: {
  columnLabel: string;
  agents: Agent[];
  squadId: string;
  token: string;
  targetStatus: TaskStatus;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [assignee, setAssignee] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title={`New task → ${columnLabel}`} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel="Create task"
        submitDisabled={title.trim() === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            const created = await apiPost<Task>(`/squads/${squadId}/board/tasks`, token, {
              title: title.trim(),
              description: description.trim(),
              assignee_agent_id: assignee,
            });
            if (targetStatus !== "todo") {
              await apiPost(`/tasks/${created.id}/move`, token, { status: targetStatus });
            }
            onCreated();
          } catch (err) {
            setError(err instanceof Error ? err.message : "create failed");
            setBusy(false);
          }
        }}
      >
        <label className="field">
          <span>Title</span>
          <input value={title} onChange={(e) => setTitle(e.target.value)} autoFocus placeholder="What needs doing?" />
        </label>
        <label className="field">
          <span>Description</span>
          <textarea value={description} onChange={(e) => setDescription(e.target.value)} />
        </label>
        <label className="field">
          <span>Assignee</span>
          <select value={assignee} onChange={(e) => setAssignee(e.target.value)}>
            <option value="">— unassigned —</option>
            {agents.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
              </option>
            ))}
          </select>
          {agents.length === 0 ? <span className="field-hint">No agents in this squad yet.</span> : null}
        </label>
      </ModalForm>
    </Modal>
  );
}

function ColumnConfigModal({
  squad,
  token,
  onClose,
  onSaved,
}: {
  squad: Squad;
  token: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [cols, setCols] = useState<BoardColumnConfig[]>(() => boardColumnsFromOperatingModel(squad.operating_model));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title="Configure board columns" onClose={onClose}>
      <p style={{ marginTop: 0, color: "var(--ink-muted)", fontSize: "var(--text-sm)" }}>
        Reorder, rename, or hide columns. The underlying task lifecycle (todo → in-progress → in-review → done, plus
        blocked) is fixed by the agent runtime — configuration changes presentation only.
      </p>
      {cols.map((col, i) => (
        <div key={col.status} className="field-row" style={{ alignItems: "center", marginBottom: "var(--space-2)" }}>
          <label style={{ display: "flex", alignItems: "center", gap: "var(--space-2)", fontSize: "var(--text-sm)" }}>
            <input
              type="checkbox"
              checked={col.visible}
              onChange={(e) =>
                setCols((prev) => prev.map((c) => (c.status === col.status ? { ...c, visible: e.target.checked } : c)))
              }
            />
            <input
              value={col.label}
              onChange={(e) =>
                setCols((prev) => prev.map((c) => (c.status === col.status ? { ...c, label: e.target.value } : c)))
              }
              style={{
                border: "1px solid var(--line-strong)",
                borderRadius: "var(--radius-md)",
                padding: "var(--space-1) var(--space-2)",
                fontSize: "var(--text-sm)",
                width: "100%",
              }}
            />
          </label>
          <div style={{ display: "flex", gap: "var(--space-1)", justifyContent: "flex-end" }}>
            <button
              type="button"
              className="btn btn-sm"
              disabled={i === 0}
              onClick={() => setCols((prev) => moveColumn(prev, col.status, -1))}
            >
              ↑
            </button>
            <button
              type="button"
              className="btn btn-sm"
              disabled={i === cols.length - 1}
              onClick={() => setCols((prev) => moveColumn(prev, col.status, 1))}
            >
              ↓
            </button>
          </div>
        </div>
      ))}
      {error ? <div className="notice error">{error}</div> : null}
      <div className="modal-foot">
        <button type="button" className="btn" onClick={onClose} disabled={busy}>
          Cancel
        </button>
        <button
          type="button"
          className="btn btn-primary"
          disabled={busy || cols.every((c) => !c.visible)}
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              const model = { ...boardOperatingModel(squad.operating_model), board: { columns: cols } };
              await apiPatch<Squad>(`/squads/${squad.id}`, token, { operating_model: model });
              onSaved();
            } catch (err) {
              setError(err instanceof Error ? err.message : "save failed");
              setBusy(false);
            }
          }}
        >
          {busy ? "Saving…" : "Save columns"}
        </button>
      </div>
    </Modal>
  );
}

function boardOperatingModel(raw: unknown): Record<string, unknown> {
  if (typeof raw === "string") {
    try {
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : {};
    } catch {
      return {};
    }
  }
  if (raw && typeof raw === "object" && !Array.isArray(raw)) return raw as Record<string, unknown>;
  return {};
}
