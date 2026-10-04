// S-184: short human-referenceable task refs (Kanbunny "S-xxx" style).
// The control-plane assigns an immutable per-squad sequential task_number
// (migration 0027); the UI renders it as "T-<n>" on board cards, the task
// screen title, and the breadcrumb trail.

import type { Task } from "./api";

// formatTaskRef returns the short "T-<n>" reference for a task, or "" when
// the task is missing or predates numbering (task_number unset/zero).
export function formatTaskRef(task: Pick<Task, "task_number"> | null | undefined): string {
  if (!task?.task_number) return "";
  return `T-${task.task_number}`;
}
