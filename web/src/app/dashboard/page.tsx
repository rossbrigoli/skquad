"use client";

// Dashboard (S-116) — landing page. One aggregated GET /dashboard call
// powers squads + agents, costs, provider liveness and the resource
// overview. Platform admins see everything (scope "all"); everyone else
// sees owned + granted squads.
//
// S-190 adds GET /dashboard/usage: MTD cost tiles (plus platform-wide
// cost/users/agents for admins), daily stacked-bar histograms per squad
// and per agent with a tokens/cost toggle, and month-to-date cost + token
// counts per provider and per model in the AI Providers section.

import { useState } from "react";
import Link from "next/link";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { BarChart } from "../../components/BarChart";
import { EmptyState } from "../../components/EmptyState";
import { MetricTile } from "../../components/MetricTile";
import { StatusChip } from "../../components/StatusChip";
import { AgentTile } from "../../components/AgentTiles";
import { ProviderTilesGrid } from "../../components/ProviderTiles";
import { useApi } from "../../lib/useApi";
import { formatCompact, formatMoney, formatMoneyCents } from "../../lib/format";
import { agentStatus } from "../../lib/status";
import {
  buildStackedChart,
  costByAgent,
  costBySquad,
  providerUsageMap,
  type ChartMode,
  type ChartSource,
  type DashboardUsagePayload,
} from "../../lib/usage";
import {
  dashboardTotals,
  resourceChip,
  taskCount,
  type DashboardPayload,
  type DashboardResource,
  type DashboardSquad,
  type DashboardTotals,
} from "../../lib/dashboard";

const POLL_MS = 30_000;

// S-189/S3776: helpers extracted from DashboardPage — identical
// behaviour, smaller cognitive surface in the page component.
function makeValueFormatter(mode: ChartMode, currency: string): (n: number) => string {
  return mode === "cost"
    ? (n: number) => formatMoney(n, currency)
    : (n: number) => formatCompact(n);
}

interface UsageCharts {
  readonly squads: ReturnType<typeof buildStackedChart>;
  readonly agents: ReturnType<typeof buildStackedChart>;
}

function buildUsageCharts(usage: DashboardUsagePayload | null, mode: ChartMode): UsageCharts {
  const days = usage?.days ?? [];
  return {
    squads: buildStackedChart(days, usage?.by_squad ?? [], mode),
    agents: buildStackedChart(days, usage?.by_agent ?? [], mode),
  };
}

export default function DashboardPage() {
  const { data, loading, error, refresh } = useApi<DashboardPayload>("/dashboard", POLL_MS);
  const { data: usage, error: usageError } = useApi<DashboardUsagePayload>("/dashboard/usage?days=30", POLL_MS);
  const [mode, setMode] = useState<ChartMode>("tokens");
  const [source, setSource] = useState<ChartSource>("squads");
  const totals = dashboardTotals(data);
  const isAdmin = data?.scope === "all";
  const currency = usage?.currency ?? "USD";
  const formatValue = makeValueFormatter(mode, currency);
  const charts = buildUsageCharts(usage, mode);
  const usageChart = source === "agents" ? charts.agents : charts.squads;
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
            <MetricGrid totals={totals} isAdmin={isAdmin} usage={usage} currency={currency} />

            {/* S-201: the old per-squad + per-agent histograms are now ONE
                chart with two sliding toggles: which series (Squads |
                Agents) and which metric (Tokens | Cost $). The window is
                the last 30 days. */}
            <div className="chart-header">
              <h2 className="section-title">Daily Usage</h2>
              <div className="chart-toggles">
                <SourceToggle source={source} onChange={setSource} />
                <ModeToggle mode={mode} onChange={setMode} />
              </div>
            </div>
            <div className="card chart-card">
              <BarChart model={usageChart} formatValue={formatValue} />
            </div>

            <SquadsSection squads={data?.squads} usage={usage} currency={currency} />

            <ProvidersSection providers={data?.providers} providerUsage={providerUsage} currency={currency} />

            <ResourcesSection resources={data?.resources} />
          </>
        )}
      </AppShell>
    </AuthGate>
  );
}

// S-189/S3776: the three list sections extracted from DashboardPage.
// S-230: the Squads section renders each squad as a block whose agents
// are the SAME tiles used on the squad Overview screen (S-211), now with
// the rolling 30-day cost on every tile. The squad header carries a
// "running" chip whenever at least one of its agents is running.
function SquadsSection({
  squads,
  usage,
  currency,
}: {
  readonly squads?: DashboardSquad[];
  readonly usage: DashboardUsagePayload | null;
  readonly currency: string;
}) {
  const squadCost = costBySquad(usage);
  const agentCost = costByAgent(usage);
  return (
    <>
      <h2 className="section-title">Squads</h2>
      {(squads?.length ?? 0) === 0 ? (
        <EmptyState title="No squads yet" hint="Create a squad to see it here with its task counts and costs." />
      ) : (
        squads!.map((squad) => (
          <SquadBlock key={squad.id} squad={squad} squadCost30d={squadCost.get(squad.id) ?? 0} agentCost={agentCost} currency={currency} />
        ))
      )}
    </>
  );
}

function SquadBlock({
  squad,
  squadCost30d,
  agentCost,
  currency,
}: {
  readonly squad: DashboardSquad;
  readonly squadCost30d: number;
  readonly agentCost: ReadonlyMap<string, number>;
  readonly currency: string;
}) {
  const anyRunning = (squad.agents ?? []).some((agent) => agentStatusOf(agent) === "running");
  return (
    <section className="squad-block">
      <div className="squad-block-header">
        <Link href={`/squads/${squad.id}`} className="entity-title">
          {squad.name}
        </Link>
        {anyRunning ? <StatusChip status="running" /> : null}
        <span className="squad-block-cost mono">{formatMoney(squadCost30d, currency)}</span>
      </div>
      <div className="entity-meta">
        {taskCount(squad, "todo")} todo · {taskCount(squad, "in-progress")} in progress
        {squad.owner_name ? ` · owner: ${squad.owner_name}` : ""}
      </div>
      {(squad.agents?.length ?? 0) > 0 ? (
        <div className="agent-tile-grid" style={{ marginTop: "var(--space-2)" }}>
          {squad.agents!.map((agent) => (
            <AgentTile
              key={agent.id}
              agent={agent}
              href={`/squads/${squad.id}/agents/${agent.id}`}
              costLabel={formatMoney(agentCost.get(agent.id) ?? 0, currency)}
              variant="dashboard"
            />
          ))}
        </div>
      ) : (
        <div className="entity-meta" style={{ marginTop: "var(--space-2)" }}>
          no agents in this squad
        </div>
      )}
    </section>
  );
}

// S-230: providers render as tiles (same anatomy as the agent tiles)
// with a green ONLINE indicator and the aggregated last-30-days cost
// across all of the provider's AI models.
function ProvidersSection({
  providers,
  providerUsage,
  currency,
}: {
  readonly providers?: DashboardPayload["providers"];
  readonly providerUsage: ReturnType<typeof providerUsageMap>;
  readonly currency: string;
}) {
  return (
    <>
      <h2 className="section-title">AI Providers</h2>
      {(providers?.length ?? 0) === 0 ? (
        <EmptyState title="No providers registered" hint="Register providers in Settings → AI Models." />
      ) : (
        <ProviderTilesGrid providers={providers!} usageMap={providerUsage} currency={currency} />
      )}
    </>
  );
}

function ResourcesSection({ resources }: { readonly resources?: DashboardPayload["resources"] }) {
  return (
    <>
      <h2 className="section-title">Resources</h2>
      {(resources?.length ?? 0) === 0 ? (
        <EmptyState title="No resources registered" hint="Skills, tools, APIs, knowledge bases and workspaces appear here." />
      ) : (
        <div className="entity-list">
          {resources!.map((resource) => (
            <ResourceRow key={`${resource.type}-${resource.id}`} resource={resource} />
          ))}
        </div>
      )}
    </>
  );
}

// S-189/S3776: the tiles grid extracted from DashboardPage (cognitive
// complexity split). Behaviour is unchanged from the inline version.
function MetricGrid({
  totals,
  isAdmin,
  usage,
  currency,
}: {
  readonly totals: DashboardTotals;
  readonly isAdmin: boolean;
  readonly usage: DashboardUsagePayload | null;
  readonly currency: string;
}) {
  return (
    <div className="metric-grid">
      <MetricTile label="Squads" value={totals.squads} sub={isAdmin ? "all squads (platform admin)" : "owned + granted"} />
      <MetricTile label="Agents running" value={totals.agentsRunning} sub={`${totals.agents} agents total`} />
      <MetricTile
        label="Agents in error"
        value={totals.agentsError}
        attention={totals.agentsError > 0}
        sub={totals.agentsError > 0 ? "check the squads below" : "all healthy"}
      />
      <MetricTile label="Total cost" value={formatMoneyCents(totals.totalCost, currency)} sub="across your squads" />
      <MetricTile
        label="MTD cost"
        value={formatMoneyCents(usage?.squad_mtd_cost ?? 0, currency)}
        sub={isAdmin ? "this month, all squads" : "this month, your squads"}
      />
      {isAdmin && usage?.platform ? (
        <>
          <MetricTile label="Platform total cost" value={formatMoneyCents(usage.platform.total_cost, currency)} sub="all time, all squads" />
          <MetricTile label="Platform MTD cost" value={formatMoneyCents(usage.platform.mtd_cost, currency)} sub="this month, all squads" />
          <MetricTile label="Users" value={usage.platform.users} sub="in the platform" />
          <MetricTile label="Agents" value={usage.platform.agents} sub="across all squads" />
        </>
      ) : null}
    </div>
  );
}

// ResourceRow renders one registered resource line.
function ResourceRow({ resource }: { readonly resource: DashboardResource }) {
  const chip = resourceChip(resource.status);
  return (
    <div className="entity-row">
      <div className="entity-main">
        <span className="entity-title">{resource.name}</span>
        <span className="entity-meta">{resource.type.replaceAll("_", " ")}</span>
      </div>
      <div className="entity-side">
        <span className={chip.className}>{chip.label}</span>
      </div>
    </div>
  );
}

// SlideSwitch is the shared iOS-style sliding switch (S-195 pattern,
// generalized in S-201 so the chart can carry two of them). The knob
// slides behind the active label; both labels stay visible.
export function SlideSwitch({
  leftLabel,
  rightLabel,
  isRight,
  onChange,
  ariaLabel,
}: {
  readonly leftLabel: string;
  readonly rightLabel: string;
  readonly isRight: boolean;
  readonly onChange: (right: boolean) => void;
  readonly ariaLabel: string;
}) {
  const toggle = () => onChange(!isRight);
  return (
    <div
      className="switch-toggle"
      role="switch"
      aria-checked={isRight}
      aria-label={ariaLabel}
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
        <span className={isRight ? undefined : "switch-label-active"}>{leftLabel}</span>
        <span className={isRight ? "switch-label-active" : undefined}>{rightLabel}</span>
      </span>
      <span className="switch-knob" aria-hidden="true" />
    </div>
  );
}

// ModeToggle flips the chart between the token and dollar series with the
// iOS-style sliding switch (S-195: replaces the old two-button control).
export function ModeToggle({ mode, onChange }: { readonly mode: ChartMode; readonly onChange: (mode: ChartMode) => void }) {
  return (
    <SlideSwitch
      leftLabel="Tokens"
      rightLabel="Cost ($)"
      isRight={mode === "cost"}
      onChange={(right) => onChange(right ? "cost" : "tokens")}
      ariaLabel={mode === "cost" ? "Chart metric: cost" : "Chart metric: tokens"}
    />
  );
}

// SourceToggle (S-201) switches the single Daily Usage chart between the
// per-squad and per-agent series — the second sliding switch beside the
// tokens/cost control.
export function SourceToggle({
  source,
  onChange,
}: {
  readonly source: ChartSource;
  readonly onChange: (source: ChartSource) => void;
}) {
  return (
    <SlideSwitch
      leftLabel="Squads"
      rightLabel="Agents"
      isRight={source === "agents"}
      onChange={(right) => onChange(right ? "agents" : "squads")}
      ariaLabel={source === "agents" ? "Chart series: agents" : "Chart series: squads"}
    />
  );
}

// agentStatusOf mirrors lib/status.agentStatus for dashboard agent rows
// (the API shape here is DashboardAgent, not the full Agent type).
function agentStatusOf(agent: { status?: string }) {
  return agentStatus({ id: "", squad_id: "", name: "", status: agent.status });
}
