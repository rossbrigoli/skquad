// S-211: squad work-in-flight counting. Fixes the Overview metric tile
// where blocked tasks were counted as "open" (open used to be
// `tasks.length - done.length`, which swept blocked items in).
//
// Buckets (mutually exclusive for open/blocked/done):
//   done    — status "done"
//   blocked — status "blocked"
//   open    — everything else (to-do, in-review, running, stalled…)
//
// Ross's example: 3 cards, 1 done, 2 blocked → open=0, blocked=2.

import type { Task } from "./api";

export interface WorkInFlight {
  readonly open: number;
  readonly blocked: number;
  readonly done: number;
}

export function workInFlight(tasks: readonly Task[] | null | undefined): WorkInFlight {
  let open = 0;
  let blocked = 0;
  let done = 0;
  for (const task of tasks ?? []) {
    if (task.status === "done") done += 1;
    else if (task.status === "blocked") blocked += 1;
    else open += 1;
  }
  return { open, blocked, done };
}

// buildContextSavePayload (S-211): the Squad Context tab's single
// "Save context" action persists BOTH the mission and the squad context
// in one PATCH. Mission keeps its own field — it is never merged into
// the context text; the control plane composes mission-on-top at
// injection time (see control-plane/internal/httpapi/prompt_handlers.go).
export function buildContextSavePayload(
  mission: string,
  prompt: string,
): { mission: string; prompt: string } {
  return { mission: mission.trim(), prompt };
}
