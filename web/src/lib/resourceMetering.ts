// TG-9 slice C: per-RESOURCE metering rollups — pure, browser-independent.
//
// Backend reality (reported gap, deliberately NOT worked around by
// changing the backend): the control-plane metering API only aggregates
// per squad (GET /squads/{id}/metering), per agent
// (GET /agents/{id}/metering?since=) and platform-wide
// (GET /metering/summary). There is NO raw metering-event list endpoint
// and NO per-resource aggregation, so tokens/cost cannot be attributed
// to a resource from the API today.
//
// Backend gap (S-268 note: still pending — re-file when actionable):
// once the control plane exposes per-resource metering
// (or a raw metering list endpoint), replace this audit-derived rollup
// with the real tokens/cost numbers. Until then this module aggregates
// what IS observable — per-resource call/decision counts from the audit
// list endpoint — and the UI labels the token/cost columns as
// unavailable rather than showing misleading zeros.

import type { AuditEntry } from "./api";
import { deriveDecision } from "./audit";

export type ResourceCallRollup = {
  key: string; // `${resource_type}:${resource_id}`
  resourceType: string;
  resourceId: string;
  calls: number;
  allowed: number;
  denied: number;
  pending: number;
  lastSeen: string; // ISO timestamp of the newest event ("" when none carried one)
};

export type RollupTotals = {
  resources: number;
  calls: number;
  allowed: number;
  denied: number;
  pending: number;
};

// aggregateResourceRollups buckets audit entries by (resource_type,
// resource_id) and counts decisions per resource. Entries without a
// resource reference are skipped. Sorted by call volume desc, then key
// for stable output.
export function aggregateResourceRollups(entries: AuditEntry[]): ResourceCallRollup[] {
  const buckets = new Map<string, ResourceCallRollup>();
  for (const entry of entries) {
    const type = (entry.resource_type ?? "").trim();
    const id = (entry.resource_id ?? "").trim();
    if (!type || !id) continue;
    const key = `${type}:${id}`;
    let row = buckets.get(key);
    if (!row) {
      row = {
        key,
        resourceType: type,
        resourceId: id,
        calls: 0,
        allowed: 0,
        denied: 0,
        pending: 0,
        lastSeen: "",
      };
      buckets.set(key, row);
    }
    row.calls += 1;
    const { kind } = deriveDecision(entry);
    if (kind === "allow") row.allowed += 1;
    else if (kind === "deny") row.denied += 1;
    else row.pending += 1;
    const ts = entry.timestamp ?? "";
    if (ts && ts > row.lastSeen) row.lastSeen = ts;
  }
  return [...buckets.values()].sort((a, b) => b.calls - a.calls || a.key.localeCompare(b.key));
}

// rollupTotals sums the per-resource rows for the summary strip.
export function rollupTotals(rows: ResourceCallRollup[]): RollupTotals {
  return rows.reduce<RollupTotals>(
    (acc, row) => ({
      resources: acc.resources + 1,
      calls: acc.calls + row.calls,
      allowed: acc.allowed + row.allowed,
      denied: acc.denied + row.denied,
      pending: acc.pending + row.pending,
    }),
    { resources: 0, calls: 0, allowed: 0, denied: 0, pending: 0 },
  );
}
