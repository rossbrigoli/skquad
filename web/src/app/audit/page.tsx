"use client";

// TG-9 slice C: Audit & Metering dashboard.
//
// Two tabs over the control-plane audit log:
//   * "Audit log" — filterable event table (agent / task / resource /
//     time range / decision) with a per-event detail drawer. Decision
//     codes render human-readable via lib/audit (TG-8 stable codes).
//   * "Resource metering" — per-resource call/decision rollups derived
//     from the same audit window. Tokens/cost are NOT attributable per
//     resource from the current API (metering aggregates per
//     squad/agent only) — the panel says so instead of faking zeros.
//
// Scoping mirrors what the backend enforces:
//   * platform_admin → GET /audit (all squads, optional squad_id filter)
//   * squad owner    → GET /squads/{id}/audit (squad select required)
// Filters beyond squad_id/limit are applied client-side over the fetched
// window (≤500 events) — see lib/audit.ts header for the backend gap.

import { useMemo, useState } from "react";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";
import { useAuth } from "../../lib/auth";
import { isPlatformAdmin } from "../../lib/aimodels";
import { useApi } from "../../lib/useApi";
import type { AuditEntry, Squad } from "../../lib/api";
import {
  AUDIT_LIMIT_MAX,
  buildAuditPath,
  deriveDecision,
  deriveTier,
  filterAuditEntries,
  formatActor,
  hasActiveFilters,
  type AuditFilters,
  type DecisionKind,
} from "../../lib/audit";
import { aggregateResourceRollups, rollupTotals } from "../../lib/resourceMetering";
import { formatRelativeTime } from "../../lib/format";

type Tab = "log" | "metering";

const EMPTY_FILTERS: AuditFilters = {
  squadId: "",
  agentId: "",
  taskId: "",
  resourceId: "",
  decision: "",
  since: "",
  until: "",
  limit: 200,
};

function DecisionChip({ kind, label }: { readonly kind: DecisionKind; readonly label: string }) {
  return <span className={`chip chip-${kind}`}>{label}</span>;
}

function FilterBar({
  filters,
  onChange,
  squads,
  isAdmin,
}: {
  readonly filters: AuditFilters;
  readonly onChange: (next: AuditFilters) => void;
  readonly squads: Squad[];
  readonly isAdmin: boolean;
}) {
  const set = (patch: Partial<AuditFilters>) => onChange({ ...filters, ...patch });
  return (
    <div className="card audit-filter-bar" role="search" aria-label="Audit filters">
      <label>
        <span>Squad</span>
        <select
          value={filters.squadId ?? ""}
          onChange={(e) => set({ squadId: e.target.value })}
          aria-label="Squad filter"
        >
          <option value="">{isAdmin ? "All squads" : "Select a squad…"}</option>
          {squads.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name}
            </option>
          ))}
        </select>
      </label>
      <label>
        <span>Agent ID</span>
        <input
          type="text"
          value={filters.agentId ?? ""}
          placeholder="agent uuid"
          onChange={(e) => set({ agentId: e.target.value })}
          aria-label="Agent filter"
        />
      </label>
      <label>
        <span>Task ID</span>
        <input
          type="text"
          value={filters.taskId ?? ""}
          placeholder="task uuid"
          onChange={(e) => set({ taskId: e.target.value })}
          aria-label="Task filter"
        />
      </label>
      <label>
        <span>Resource ID</span>
        <input
          type="text"
          value={filters.resourceId ?? ""}
          placeholder="resource uuid"
          onChange={(e) => set({ resourceId: e.target.value })}
          aria-label="Resource filter"
        />
      </label>
      <label>
        <span>Decision</span>
        <select
          value={filters.decision ?? ""}
          onChange={(e) => set({ decision: e.target.value as AuditFilters["decision"] })}
          aria-label="Decision filter"
        >
          <option value="">All</option>
          <option value="allow">Allowed</option>
          <option value="deny">Denied</option>
          <option value="pending">Pending</option>
        </select>
      </label>
      <label>
        <span>From</span>
        <input
          type="datetime-local"
          value={filters.since ?? ""}
          onChange={(e) => set({ since: e.target.value })}
          aria-label="From time"
        />
      </label>
      <label>
        <span>To</span>
        <input
          type="datetime-local"
          value={filters.until ?? ""}
          onChange={(e) => set({ until: e.target.value })}
          aria-label="To time"
        />
      </label>
      <label>
        <span>Window</span>
        <select
          value={String(filters.limit ?? 200)}
          onChange={(e) => set({ limit: Number(e.target.value) })}
          aria-label="Event window"
        >
          {[100, 200, 500].map((n) => (
            <option key={n} value={n}>
              {n === AUDIT_LIMIT_MAX ? `${n} (max)` : n}
            </option>
          ))}
        </select>
      </label>
      {hasActiveFilters(filters) ? (
        <button type="button" className="btn ghost" onClick={() => onChange({ ...EMPTY_FILTERS })}>
          Clear filters
        </button>
      ) : null}
    </div>
  );
}

// toInstant converts a datetime-local value to an ISO instant; empty or
// invalid values yield undefined (filter inactive).
function toInstant(local: string | undefined): string | undefined {
  if (!local) return undefined;
  const ms = Date.parse(local);
  return Number.isFinite(ms) ? new Date(ms).toISOString() : undefined;
}

function AuditTable({
  entries,
  onSelect,
  selectedId,
}: {
  readonly entries: AuditEntry[];
  readonly onSelect: (entry: AuditEntry) => void;
  readonly selectedId: string | null;
}) {
  return (
    <table className="audit-table">
      <thead>
        <tr>
          <th>Time</th>
          <th>Actor</th>
          <th>Resource</th>
          <th>Action / operation</th>
          <th>Decision</th>
          <th>Tier</th>
        </tr>
      </thead>
      <tbody>
        {entries.map((entry) => {
          const decision = deriveDecision(entry);
          const tier = deriveTier(entry);
          return (
            <tr
              key={entry.id}
              className={selectedId === entry.id ? "selected" : undefined}
              onClick={() => onSelect(entry)}
              tabIndex={0}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") onSelect(entry);
              }}
            >
              <td className="mono" title={entry.timestamp ?? ""}>
                {formatRelativeTime(entry.timestamp)}
              </td>
              <td>{formatActor(entry)}</td>
              <td className="mono">
                <span>{entry.resource_type}</span>{" "}
                <span title={entry.resource_id}>{entry.resource_id.slice(0, 8)}</span>
              </td>
              <td className="mono">{entry.action}</td>
              <td>
                <DecisionChip kind={decision.kind} label={decision.label} />
              </td>
              <td>{tier || "—"}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function EventDetail({ entry, onClose }: { readonly entry: AuditEntry; readonly onClose: () => void }) {
  const decision = deriveDecision(entry);
  const tier = deriveTier(entry);
  return (
    <aside className="card audit-detail" aria-label="Event detail">
      <div className="audit-detail-head">
        <h2>Event detail</h2>
        <button type="button" className="btn ghost" onClick={onClose} aria-label="Close event detail">
          ✕
        </button>
      </div>
      <dl>
        <dt>Timestamp</dt>
        <dd className="mono">{entry.timestamp ?? "—"}</dd>
        <dt>Actor</dt>
        <dd>
          {formatActor(entry)} <span className="entity-meta">({entry.actor_type})</span>
        </dd>
        <dt>Action</dt>
        <dd className="mono">{entry.action}</dd>
        <dt>Resource</dt>
        <dd className="mono">
          <span>{entry.resource_type}</span> <span>{entry.resource_id}</span>
        </dd>
        <dt>Squad</dt>
        <dd className="mono">{entry.squad_id || "—"}</dd>
        <dt>Decision</dt>
        <dd>
          <DecisionChip kind={decision.kind} label={decision.label} />{" "}
          {decision.code ? <span className="mono entity-meta">{decision.code}</span> : null}
        </dd>
        <dt>Tier</dt>
        <dd>{tier || "—"}</dd>
        <dt>Metadata</dt>
        <dd>
          <pre className="mono audit-meta">{JSON.stringify(entry.metadata ?? {}, null, 2)}</pre>
        </dd>
      </dl>
    </aside>
  );
}

function ResourceMeteringPanel({ entries }: { readonly entries: AuditEntry[] }) {
  const rows = aggregateResourceRollups(entries);
  const totals = rollupTotals(rows);
  if (rows.length === 0) {
    return <EmptyState title="No resource activity" hint="No audited events in the current window." />;
  }
  return (
    <>
      <p className="entity-meta">
        Aggregated client-side from the audit window ({totals.calls} events across {totals.resources}{" "}
        resources). Tokens and cost are not attributable per resource yet — the metering API only
        aggregates per squad/agent (backend gap; see <span className="mono">docs/observability-metering.md</span>).
      </p>
      <table className="audit-table">
        <thead>
          <tr>
            <th>Resource</th>
            <th>Calls</th>
            <th>Allowed</th>
            <th>Denied</th>
            <th>Pending</th>
            <th>Tokens / cost</th>
            <th>Last seen</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.key}>
              <td className="mono">
                <span>{row.resourceType}</span>{" "}
                <span title={row.resourceId}>{row.resourceId.slice(0, 8)}</span>
              </td>
              <td>{row.calls}</td>
              <td>{row.allowed}</td>
              <td>{row.denied}</td>
              <td>{row.pending}</td>
              <td className="entity-meta">n/a</td>
              <td className="mono" title={row.lastSeen}>
                {row.lastSeen ? formatRelativeTime(row.lastSeen) : "—"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

export default function AuditPage() {
  const { user } = useAuth();
  const isAdmin = isPlatformAdmin(user?.role);
  const [tab, setTab] = useState<Tab>("log");
  const [filters, setFilters] = useState<AuditFilters>({ ...EMPTY_FILTERS });
  const [selected, setSelected] = useState<AuditEntry | null>(null);

  const squads = useApi<Squad[]>(isAdmin ? "/squads?all=true" : "/squads");

  // Only squad_id/limit reach the server; the rest is client-side.
  const apiPath = useMemo(
    () =>
      buildAuditPath(
        {
          squadId: filters.squadId,
          limit: filters.limit,
        },
        isAdmin,
      ),
    [filters.squadId, filters.limit, isAdmin],
  );
  const audit = useApi<AuditEntry[]>(apiPath);

  const effectiveFilters = useMemo<AuditFilters>(
    () => ({ ...filters, since: toInstant(filters.since), until: toInstant(filters.until) }),
    [filters],
  );
  const visible = useMemo(
    () => filterAuditEntries(audit.data ?? [], effectiveFilters),
    [audit.data, effectiveFilters],
  );

  const needsSquad = !isAdmin && !(filters.squadId ?? "").trim();

  function renderContent() {
    if (needsSquad) {
      return <EmptyState title="Pick a squad" hint="Squad audit history is scoped to squads you own." />;
    }
    if (audit.loading && !audit.data) {
      return <EmptyState title="Loading audit log…" hint="Fetching recent audited events." />;
    }
    if (audit.error) {
      return <div className="notice error">{audit.error}</div>;
    }
    if (tab === "metering") {
      return <ResourceMeteringPanel entries={visible} />;
    }
    if (visible.length === 0) {
      return <EmptyState title="No matching events" hint="Widen the time window or clear filters." />;
    }
    return (
      <div className="audit-layout">
        <AuditTable
          entries={visible}
          selectedId={selected?.id ?? null}
          onSelect={(entry) => setSelected(entry)}
        />
        {selected ? <EventDetail entry={selected} onClose={() => setSelected(null)} /> : null}
      </div>
    );
  }

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Audit &amp; Metering</h1>
        <div className="tabs">
          <button type="button" className={tab === "log" ? "active" : ""} onClick={() => setTab("log")}>
            Audit log
          </button>
          <button
            type="button"
            className={tab === "metering" ? "active" : ""}
            onClick={() => setTab("metering")}
          >
            Resource metering
          </button>
        </div>
        <FilterBar
          filters={filters}
          onChange={setFilters}
          squads={squads.data ?? []}
          isAdmin={isAdmin}
        />
        {renderContent()}
      </AppShell>
    </AuthGate>
  );
}
