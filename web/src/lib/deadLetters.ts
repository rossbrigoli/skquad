// S-227: dead-letter bulk-action client logic.
//
// Pure, browser-independent selection helpers for the admin Dead letters
// screen, mirroring the S-214 prompt-template multi-select pattern. The
// bulk actions (Replay / Delete) fan out over the existing per-item
// endpoints (POST .../replay, DELETE ...) with Promise.allSettled so one
// failing message never aborts the batch; `summarizeBulkResults` turns
// the settled results into a per-item report for the UI.

export type BulkItemResult = {
  id: string;
  ok: boolean;
  error?: string;
};

export type BulkSummary = {
  total: number;
  succeeded: number;
  failed: number;
  failedIds: string[];
};

// toggleDeadLetterSelection flips one id in/out of the selection,
// returning a new Set (never mutates the input).
export function toggleDeadLetterSelection(
  selected: ReadonlySet<string>,
  id: string,
): Set<string> {
  const next = new Set(selected);
  if (next.has(id)) {
    next.delete(id);
  } else {
    next.add(id);
  }
  return next;
}

// S-189/S2301: two explicit methods instead of a boolean flag.
// allDeadLetterIds returns every message id; emptyDeadLetterSelection
// returns an empty selection.
export function allDeadLetterIds(items: readonly { id: string }[]): Set<string> {
  return new Set(items.map((m) => m.id));
}

export function emptyDeadLetterSelection(): Set<string> {
  return new Set();
}

// pruneDeadLetterSelection drops selected ids that no longer exist in
// the live list (e.g. after a refresh or a bulk delete).
export function pruneDeadLetterSelection(
  selected: ReadonlySet<string>,
  items: readonly { id: string }[],
): Set<string> {
  const live = new Set(items.map((m) => m.id));
  const next = new Set<string>();
  for (const id of selected) {
    if (live.has(id)) next.add(id);
  }
  return next;
}

// summarizeBulkResults condenses per-item results into counts plus the
// ids that failed, for a single honest note line.
export function summarizeBulkResults(results: readonly BulkItemResult[]): BulkSummary {
  const failedIds = results.filter((r) => !r.ok).map((r) => r.id);
  return {
    total: results.length,
    succeeded: results.length - failedIds.length,
    failed: failedIds.length,
    failedIds,
  };
}

// runBulkAction runs `fn` for each id with Promise.allSettled semantics
// and maps outcomes to BulkItemResult[]. A rejection becomes
// { ok: false, error } instead of throwing.
export async function runBulkAction(
  ids: readonly string[],
  fn: (id: string) => Promise<unknown>,
): Promise<BulkItemResult[]> {
  const settled = await Promise.allSettled(ids.map((id) => fn(id)));
  return settled.map((res, i) =>
    res.status === "fulfilled"
      ? { id: ids[i], ok: true }
      : {
          id: ids[i],
          ok: false,
          error: res.reason instanceof Error ? res.reason.message : String(res.reason),
        },
  );
}
