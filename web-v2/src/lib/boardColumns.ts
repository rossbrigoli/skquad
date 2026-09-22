import type { TaskStatus } from "./api";

// The canonical Kanban lifecycle is fixed at the engine level: agents claim,
// complete, and block against these exact statuses (control-plane
// domain.TaskStatus). Per-squad configuration may reorder, rename, and hide
// columns, but cannot introduce new statuses without breaking the agent
// runtime contract. See docs/KANBAN-BOARD-DESIGN rationale in UIv2-11 card.
export const CANONICAL_STATUSES: TaskStatus[] = ["todo", "in-progress", "in-review", "done", "blocked"];

export const DEFAULT_COLUMN_LABELS: Record<TaskStatus, string> = {
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

// Merge persisted column config with defaults. Unknown statuses are dropped,
// missing columns are appended in canonical order, labels fall back to
// defaults when blank. Never throws — malformed input yields defaults.
export function normalizeColumns(raw: unknown): BoardColumnConfig[] {
  if (!Array.isArray(raw)) return defaultColumns();
  const out: BoardColumnConfig[] = [];
  const seen = new Set<TaskStatus>();
  for (const item of raw) {
    if (!item || typeof item !== "object") continue;
    const rec = item as Record<string, unknown>;
    if (!isTaskStatus(rec.status)) continue;
    if (seen.has(rec.status)) continue;
    seen.add(rec.status);
    const label = typeof rec.label === "string" && rec.label.trim() !== "" ? rec.label : DEFAULT_COLUMN_LABELS[rec.status];
    out.push({ status: rec.status, label, visible: rec.visible !== false });
  }
  for (const status of CANONICAL_STATUSES) {
    if (!seen.has(status)) out.push({ status, label: DEFAULT_COLUMN_LABELS[status], visible: true });
  }
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
