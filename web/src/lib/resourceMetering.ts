// S-269: per-RESOURCE metering from the real control-plane endpoint.
//
// Replaces the TG-9 slice C audit-derived rollup: the control plane now
// exposes per-resource usage (GET /api/v1/metering/resources and
// GET /api/v1/resources/{id}/metering), aggregated from metering
// events carrying the resource dimension. Metering is OPTIONAL per
// resource: `metered=false` rows report an EXPLICIT zero cost (every
// call to a non-metered resource costs 0), so a zero is a billed
// fact, not missing data.
//
// Pure/browser-independent like the rest of lib: parsing, totals and
// path building only — the UI wires useApi/fetch around it.

export type ResourceMeteringRow = {
  resourceId: string;
  resourceType: string;
  name: string;
  metered: boolean;
  calls: number;
  cost: number;
  currency: string;
  lastSeen: string; // ISO timestamp ("" when the API carried none)
};

export type ResourceMeteringTotals = {
  resources: number;
  calls: number;
  cost: number;
  currency: string;
};

// resourceMeteringPath builds the single-resource endpoint path.
// `since` is an optional RFC3339 lower bound (absent = all time).
export function resourceMeteringPath(resourceId: string, since?: string): string {
  const params = new URLSearchParams();
  if (since) params.set("since", since);
  const qs = params.toString();
  return `/resources/${encodeURIComponent(resourceId)}/metering${qs ? `?${qs}` : ""}`;
}

// listResourceMeteringPath builds the platform list endpoint path with
// optional since/type filters.
export function listResourceMeteringPath(since?: string, type?: string): string {
  const params = new URLSearchParams();
  if (since) params.set("since", since);
  if (type) params.set("type", type);
  const qs = params.toString();
  return `/metering/resources${qs ? `?${qs}` : ""}`;
}

function num(value: unknown, fallback = 0): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

// parseResourceMeteringRow normalises one wire row (snake_case CP
// shape). Malformed or missing numeric fields degrade to 0 rather than
// poisoning the table.
export function parseResourceMeteringRow(raw: unknown): ResourceMeteringRow | null {
  if (typeof raw !== "object" || raw === null) return null;
  const o = raw as Record<string, unknown>;
  const resourceId = str(o.resource_id).trim();
  if (resourceId === "") return null;
  return {
    resourceId,
    resourceType: str(o.resource_type),
    name: str(o.name),
    metered: o.metered === true,
    calls: num(o.calls),
    cost: num(o.cost),
    currency: str(o.currency) || "USD",
    lastSeen: str(o.last_seen),
  };
}

// parseResourceMeteringRows accepts the list endpoint envelope
// ({ rows: [...] }) or a bare array and returns normalised rows,
// dropping unparseable entries. Sorted by calls desc, then id for
// stable rendering.
export function parseResourceMeteringRows(payload: unknown): ResourceMeteringRow[] {
  let list: unknown = payload;
  if (payload && typeof payload === "object" && !Array.isArray(payload)) {
    list = (payload as Record<string, unknown>).rows ?? [];
  }
  if (!Array.isArray(list)) return [];
  const rows = list
    .map(parseResourceMeteringRow)
    .filter((r): r is ResourceMeteringRow => r !== null);
  return rows.sort((a, b) => b.calls - a.calls || a.resourceId.localeCompare(b.resourceId));
}

// resourceMeteringTotals sums the per-resource rows for the summary
// strip. Currency is the first non-empty row currency (the platform
// bills in one currency; mixed currencies would be a data bug).
export function resourceMeteringTotals(rows: ResourceMeteringRow[]): ResourceMeteringTotals {
  return rows.reduce<ResourceMeteringTotals>(
    (acc, row) => ({
      resources: acc.resources + 1,
      calls: acc.calls + row.calls,
      cost: acc.cost + row.cost,
      currency: acc.currency || row.currency,
    }),
    { resources: 0, calls: 0, cost: 0, currency: "" },
  );
}
