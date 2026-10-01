"use client";

// Dashboard (S-116) — landing page. One aggregated GET /dashboard call
// powers squads + agents, costs, provider liveness and the resource
// overview. Platform admins see everything (scope "all"); everyone else
// sees owned + granted squads.
//
// S-190 adds GET /dashboard/usage: MTD cost tiles (plus platform-wide
// cost/users/agents for admins), daily stacked-bar histograms per squad
// and per agent with a tokens/cost toggle, and month-to-date cost + token
// counts per provider and per model in the LLM Providers section.

import { useState } from "react";
import Link from "next/link";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { BarChart } from "../../components/BarChart";
import { EmptyState } from "../../components/EmptyState";
import { MetricTile } from "../../components/MetricTile";
import { StatusChip } from "../../components/StatusChip";
import { useApi } from "../../lib/useApi";
import { formatCompact, formatCost, formatMoney, formatTokens } from "../../lib/format";
import { agentStatus } from "../../lib/status";
import {
  buildStackedChart,
  providerUsageMap,
  type ChartMode,
  type DashboardUsagePayload,
} from "../../lib/usage";
import {
  dashboardTotals,
  providerChip,
  resourceChip,
  taskCount,
  type DashboardPayload,
} from "../../lib/dashboard";

const POLL_MS = 30_000;

export default function DashboardPage() {
  const { data, loading, error, refresh } = useApi<DashboardPayload>("/dashboard", POLL_MS);
  const { data: usage, error: usageError } = useApi<DashboardUsagePayload>("/dashboard/usage", POLL_MS);
  const [mode, setMode] = useState<ChartMode>("tokens");
  const totals = dashboardTotals(data);
  const isAdmin = data?.scope === "all";
  const currency = usage?.currency ?? "USD";
  const formatValue = mode === "cost" ? (n: number) => formatMoney(n, currency) : (n: number) => formatCompact(n);
  const squadChart = buildStackedChart(usage?.days ?? [], usage?.by_squad ?? [], mode);
  const agentChart = buildStackedChart(usage?.days ?? [], usage?.by_agent ?? [], mode);
  const providerUsage = providerUsageMap(usage?.providers);

  return (
    <AuthGate>
      <AppShell>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
          <h1 className="page-title">Dashboard</h1>
          <button type="button" className="btn" onClick={refresh}>
            Refresh
          </button>
        </div>
        {error ? <div className="notice error">{error}</div> : null}
        {usageError ? <div className="notice error">Usage series unavailable: {usageError}</div> : null}
        {loading && !data ? (
          <EmptyState title="Loading your dashboard…" hint="Aggregating squads, agents, costs, providers and resources." />
        ) : (
          <>
            <div className="metric-grid">
              <MetricTile label="Squads" value={totals.squads} sub={isAdmin ? "all squads (platform admin)" : "owned + granted"} />
              <MetricTile label="Agents running" value={totals.agentsRunning} sub={`${totals.agents} agents total`} />
              <MetricTile
                label="Agents in error"
                value={totals.agentsError}
                attention={totals.agentsError > 0}
                sub={totals.agentsError > 0 ? "check the squads below" : "all healthy"}
              />
              <MetricTile label="Total cost" value={formatMoney(totals.totalCost, currency)} sub="across your squads" />
              <MetricTile
                label="MTD cost"
                value={formatMoney(usage?.squad_mtd_cost ?? 0, currency)}
                sub={isAdmin ? "this month, all squads" : "this month, your squads"}
              />
              {isAdmin && usage?.platform ? (
                <>
                  <MetricTile label="Platform total cost" value={formatMoney(usage.platform.total_cost, currency)} sub="all time, all squads" />
                  <MetricTile label="Platform MTD cost" value={formatMoney(usage.platform.mtd_cost, currency)} sub="this month, all squads" />
                  <MetricTile label="Users" value={usage.platform.users} sub="in the platform" />
                  <MetricTile label="Agents" value={usage.platform.agents} sub="across all squads" />
                </>
              ) : null}
            </div>

            <div className="chart-header">
              <h2 className="section-title">Daily usage — per squad (past month)</h2>
              <ModeToggle mode={mode} onChange={setMode} />
            </div>
            <div className="card">
              <BarChart model={squadChart} formatValue={formatValue} />
            </div>

            <div className="chart-header">
              <h2 className="section-title">Daily usage — per agent (past month)</h2>
              <ModeToggle mode={mode} onChange={setMode} />
            </div>
            <div className="card">
              <BarChart model={agentChart} formatValue={formatValue} />
            </div>

            <h2 className="section-title">Squads</h2>
            {(data?.squads?.length ?? 0) === 0 ? (
              <EmptyState title="No squads yet" hint="Create a squad to see it here with its task counts and costs." />
            ) : (
              <div className="entity-list">
                {data!.squads.map((squad) => (
                  <div key={squad.id} className="entity-row" style={{ flexDirection: "column", alignItems: "stretch" }}>
                    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
                      <Link href={`/squads/${squad.id}`} className="entity-title" style={{ display: "block" }}>
                        {squad.name}
                      </Link>
                      <strong>{formatCost(squad.cost ?? null)}</strong>
                    </div>
                    <div className="entity-meta" style={{ marginTop: "var(--space-1)" }}>
                      {taskCount(squad, "todo")} todo · {taskCount(squad, "in-progress")} in progress
                      {squad.owner_name ? ` · owner: ${squad.owner_name}` : ""}
                      {" · "}
                      {formatTokens(squad.cost ?? null)}
                    </div>
                    {(squad.agents?.length ?? 0) > 0 ? (
                      <div style={{ marginTop: "var(--space-2)", borderTop: "1px solid var(--line)", paddingTop: "var(--space-2)" }}>
                        {squad.agents!.map((agent) => (
                          <div
                            key={agent.id}
                            style={{ display: "flex", justifyContent: "space-between", alignItems: "center", fontSize: "var(--text-sm)", padding: "2px 0" }}
                          >
                            <Link href={`/squads/${squad.id}/agents/${agent.id}`} style={{ color: "var(--ink)" }}>
                              {agent.name}
                            </Link>
                            <span style={{ display: "flex", alignItems: "center", gap: "var(--space-2)" }}>
                              <StatusChip status={agentStatusOf(agent)} />
                              <span className="mono">{formatCost(agent.cost ?? null)}</span>
                            </span>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <div className="entity-meta" style={{ marginTop: "var(--space-2)" }}>
                        no agents in this squad
                      </div>
                    )}
                  </div>
                ))}
              </div>
            )}

            <h2 className="section-title">LLM Providers</h2>
            {(data?.providers?.length ?? 0) === 0 ? (
              <EmptyState title="No providers registered" hint="Register providers in Settings → AI Models." />
            ) : (
              <div className="entity-list">
                {data!.providers.map((provider) => {
                  const chip = providerChip(provider);
                  const usageRow = providerUsage.get(provider.id);
                  return (
                    <div key={provider.id} className="entity-row" style={{ flexDirection: "column", alignItems: "stretch" }}>
                      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
                        <div className="entity-main">
                          <span className="entity-title">{provider.name}</span>
                          <span className="entity-meta">
                            {provider.kind}
                            {provider.latency_ms ? ` · ${provider.latency_ms} ms` : ""}
                            {provider.error ? ` · ${provider.error}` : ""}
                          </span>
                        </div>
                        <div className="entity-side" style={{ display: "flex", alignItems: "center", gap: "var(--space-2)" }}>
                          {usageRow ? (
                            <span className="mono">
                              MTD {formatMoney(usageRow.cost, currency)} · {formatCompact(usageRow.tokens)} tokens
                            </span>
                          ) : (
                            <span className="entity-meta">no usage this month</span>
                          )}
                          <span className={chip.className}>{chip.label}</span>
                        </div>
                      </div>
                      {(usageRow?.models?.length ?? 0) > 0 ? (
                        <div className="provider-models">
                          {usageRow!.models.map((model) => (
                            <div key={model.model} className="provider-model">
                              <span className="mono">{model.model}</span>
                              <span className="mono">
                                {formatMoney(model.cost, currency)} · {formatCompact(model.tokens)} tokens
                              </span>
                            </div>
                          ))}
                        </div>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            )}

            <h2 className="section-title">Resources</h2>
            {(data?.resources?.length ?? 0) === 0 ? (
              <EmptyState title="No resources registered" hint="Skills, tools, APIs, knowledge bases and workspaces appear here." />
            ) : (
              <div className="entity-list">
                {data!.resources.map((resource) => {
                  const chip = resourceChip(resource.status);
                  return (
                    <div key={`${resource.type}-${resource.id}`} className="entity-row">
                      <div className="entity-main">
                        <span className="entity-title">{resource.name}</span>
                        <span className="entity-meta">{resource.type.replaceAll("_", " ")}</span>
                      </div>
                      <div className="entity-side">
                        <span className={chip.className}>{chip.label}</span>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </>
        )}
      </AppShell>
    </AuthGate>
  );
}

// ModeToggle flips both histograms between token and dollar series with an
// iOS-style sliding switch (S-195: replaces the old two-button control).
export function ModeToggle({ mode, onChange }: { mode: ChartMode; onChange: (mode: ChartMode) => void }) {
  const isCost = mode === "cost";
  const toggle = () => onChange(isCost ? "tokens" : "cost");
  return (
    <div
      className="switch-toggle"
      role="switch"
      aria-checked={isCost}
      aria-label={isCost ? "Chart metric: cost" : "Chart metric: tokens"}
      tabIndex={0}
      onClick={toggle}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          toggle();
        }
      }}
    >
      <span className="switch-labels">
        <span className={isCost ? undefined : "switch-label-active"}>Tokens</span>
        <span className={isCost ? "switch-label-active" : undefined}>Cost ($)</span>
      </span>
      <span className="switch-knob" aria-hidden="true" />
    </div>
  );
}

// agentStatusOf mirrors lib/status.agentStatus for dashboard agent rows
// (the API shape here is DashboardAgent, not the full Agent type).
function agentStatusOf(agent: { status?: string }) {
  return agentStatus({ id: "", squad_id: "", name: "", status: agent.status });
}
