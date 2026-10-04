import type { TaskStatus } from "./api";

// The canonical Kanban lifecycle is fixed at the engine level: agents claim,
// complete, and block against these exact statuses (control-plane
// domain.TaskStatus). Per-squad configuration may reorder, rename, and hide
// columns, but cannot introduce new statuses without breaking the agent
// runtime contract. See docs/KANBAN-BOARD-DESIGN rationale in UIv2-11 card.
// S-213: "backlog" is the parking column left of "todo". Tasks there are
// NOT ready to start; the control plane excludes them from every agent
// pickup/listing path until a human moves them out.
export const CANONICAL_STATUSES: TaskStatus[] = ["backlog", "todo", "in-progress", "in-review", "done", "blocked"];

export const DEFAULT_COLUMN_LABELS: Record<TaskStatus, string> = {
  backlog: "Backlog",
  todo: "To do",
  "in-progress": "In progress",
  "in-review": "In review",
  done: "Done",
  blocked: "Blocked",
};

export type BoardColumnConfig = {
  status: TaskStatus;
  label: string;
  visible: boolean;
};

export function defaultColumns(): BoardColumnConfig[] {
  return CANONICAL_STATUSES.map((status) => ({
    status,
    label: DEFAULT_COLUMN_LABELS[status],
    visible: true,
  }));
}

function isTaskStatus(v: unknown): v is TaskStatus {
  return typeof v === "string" && (CANONICAL_STATUSES as string[]).includes(v);
}

function columnFromItem(item: unknown, seen: Set<TaskStatus>): BoardColumnConfig | null {
  if (!item || typeof item !== "object") {
    return null;
  }
  const rec = item as Record<string, unknown>;
  if (!isTaskStatus(rec.status) || seen.has(rec.status)) {
    return null;
  }
  const label =
    typeof rec.label === "string" && rec.label.trim() !== ""
      ? rec.label
      : DEFAULT_COLUMN_LABELS[rec.status];
  return { status: rec.status, label, visible: rec.visible !== false };
}

// S-213: squads configured before Backlog existed must get it inserted
// immediately left of "todo" (its canonical home). Handled after the
// other missing columns so the relative order stays canonical even when
// "todo" itself was absent from the persisted config.
function insertBacklog(out: BoardColumnConfig[], seen: Set<TaskStatus>): void {
  if (seen.has("backlog")) {
    return;
  }
  const backlog: BoardColumnConfig = { status: "backlog", label: DEFAULT_COLUMN_LABELS.backlog, visible: true };
  const todoIdx = out.findIndex((c) => c.status === "todo");
  if (todoIdx >= 0) {
    out.splice(todoIdx, 0, backlog);
  } else {
    out.unshift(backlog);
  }
}

// Merge persisted column config with defaults. Unknown statuses are dropped,
// missing columns are appended in canonical order, labels fall back to
// defaults when blank. Never throws — malformed input yields defaults.
export function normalizeColumns(raw: unknown): BoardColumnConfig[] {
  if (!Array.isArray(raw)) return defaultColumns();
  const out: BoardColumnConfig[] = [];
  const seen = new Set<TaskStatus>();
  for (const item of raw) {
    const col = columnFromItem(item, seen);
    if (!col) continue;
    seen.add(col.status);
    out.push(col);
  }
  for (const status of CANONICAL_STATUSES) {
    if (seen.has(status) || status === "backlog") continue;
    out.push({ status, label: DEFAULT_COLUMN_LABELS[status], visible: true });
  }
  insertBacklog(out, seen);
  return out;
}

// Parse squad operating_model JSON (free-form) into an object we can spread.
export function parseOperatingModel(raw: unknown): Record<string, unknown> {
  if (!raw) return {};
  if (typeof raw === "string") {
    try {
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : {};
    } catch {
      return {};
    }
  }
  if (typeof raw === "object" && !Array.isArray(raw)) return raw as Record<string, unknown>;
  return {};
}

export function boardColumnsFromOperatingModel(operatingModel: unknown): BoardColumnConfig[] {
  const model = parseOperatingModel(operatingModel);
  const board = model.board;
  if (!board || typeof board !== "object" || Array.isArray(board)) return defaultColumns();
  return normalizeColumns((board as Record<string, unknown>).columns);
}

export function visibleColumns(columns: BoardColumnConfig[]): BoardColumnConfig[] {
  return columns.filter((c) => c.visible);
}

// Immutably move a column one slot up/down.
export function moveColumn(columns: BoardColumnConfig[], status: TaskStatus, delta: number): BoardColumnConfig[] {
  const idx = columns.findIndex((c) => c.status === status);
  if (idx === -1) return columns;
  const target = idx + delta;
  if (target < 0 || target >= columns.length) return columns;
  const next = [...columns];
  const [item] = next.splice(idx, 1);
  next.splice(target, 0, item);
  return next;
}
