"use client";

// S-224 (S-203 WP2): Cost Management page. Renamed from "Costs" and
// rebuilt around the WP1 cost-summary + budget APIs.
//
// Tab "Cost" (everyone): running/total/MTD metric tiles, a daily cost
// stacked-bar chart switchable between squad / agent / model / provider
// groupings (reuses the dashboard BarChart + buildStackedChart), and a
// current-month-vs-my-budget bar. The backend scopes everything — the
// UI renders exactly what GET /costs/summary returns for the caller.
//
// Tab "Budget" (platform_admin only): platform knobs (default budget for
// new users, global max, optional platform-wide monthly limit) and a
// per-user budget editor with MTD spend. Non-admins never see the tab
// button and the content render is gated again as defence in depth
// (same pattern as Settings).

import { useCallback, useEffect, useState } from "react";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { BarChart } from "../../components/BarChart";
import { BudgetBar } from "../../components/BudgetBar";
import { EmptyState } from "../../components/EmptyState";
import { MetricTile } from "../../components/MetricTile";
import { AdminBudgetPanel } from "../../components/AdminBudgetPanel";
import { useAuth } from "../../lib/auth";
import { isPlatformAdmin } from "../../lib/aimodels";
import { apiGet } from "../../lib/api";
import { formatMoney } from "../../lib/format";
import { buildStackedChart } from "../../lib/usage";
import {
  COST_GROUP_SOURCES,
  resolveCostTab,
  seriesForSource,
  type CostGroupSource,
  type CostSummaryPayload,
} from "../../lib/costs";

const POLL_MS = 60_000;
const WINDOW_DAYS = 30;

// TabButton mirrors the Settings page tab control so both pages read as
// one app (S-126 styling).
function TabButton({ active, label, onClick }: { readonly active: boolean; readonly label: string; readonly onClick: () => void }) {
  return (
    <button type="button" className={active ? "active" : ""} onClick={onClick}>
      {label}
    </button>
  );
}

// GroupToggle is the segmented control above the daily chart. Four
// sources don't fit the two-way SlideSwitch, so small buttons styled to
// match the chart toggles are used instead.
function GroupToggle({
  source,
  onChange,
}: {
  readonly source: CostGroupSource;
  readonly onChange: (source: CostGroupSource) => void;
}) {
  return (
    <div className="chart-group-tabs" role="group" aria-label="Cost chart grouping">
      {COST_GROUP_SOURCES.map((g) => (
        <button
          key={g.id}
          type="button"
          className={source === g.id ? "active" : ""}
          onClick={() => onChange(g.id)}
        >
          {g.label}
        </button>
      ))}
    </div>
  );
}

function CostTabContent({
  summary,
  source,
  onSourceChange,
}: {
  readonly summary: CostSummaryPayload;
  readonly source: CostGroupSource;
  readonly onSourceChange: (source: CostGroupSource) => void;
}) {
  const currency = summary.currency ?? "USD";
  const money = (n: number) => formatMoney(n, currency);
  const model = buildStackedChart(summary.days ?? [], seriesForSource(summary, source), "cost");
  const budget = summary.budget ?? null;

  function budgetTileSub(): string {
    if (budget?.monthly_budget_usd == null) {
      return "ask a platform admin to set one";
    }
    if (budget.over_budget) {
      return "over budget";
    }
    return `${money(Math.max(0, budget.monthly_budget_usd - summary.mtd_cost))} left`;
  }

  return (
    <>
      <div className="metric-grid">
        <MetricTile label="Total cost" value={money(summary.total_cost)} sub="all time, your scope" />
        <MetricTile label="Month to date" value={money(summary.mtd_cost)} sub={`since ${summary.mtd_start}`} />
        <MetricTile
          label="Running cost"
          value={money(summary.running_cost)}
          sub="current month vs monthly budget"
          attention={Boolean(budget?.over_budget)}
        />
        <MetricTile
          label="My monthly budget"
          value={budget?.monthly_budget_usd != null ? money(budget.monthly_budget_usd) : "No limit"}
          sub={budgetTileSub()}
          attention={Boolean(budget?.over_budget)}
        />
      </div>

      <h2 className="section-title">This month vs my budget</h2>
      <div className="card chart-card">
        <BudgetBar spend={summary.mtd_cost} budget={budget?.monthly_budget_usd} currency={currency} />
      </div>

      <div className="chart-header">
        <h2 className="section-title">Daily cost by {source.replace(/s$/, "")}</h2>
        <div className="chart-toggles">
          <GroupToggle source={source} onChange={onSourceChange} />
        </div>
      </div>
      <div className="card chart-card">
        <BarChart model={model} formatValue={money} />
      </div>

      <p className="entity-meta" style={{ marginTop: "var(--space-4)" }}>
        Showing the last {summary.days?.length ?? WINDOW_DAYS} days for {summary.scope === "all" ? "all squads (platform admin view)" : "your squads"}.
      </p>
    </>
  );
}

export default function CostsPage() {
  const { token, authed, user } = useAuth();
  const isAdmin = isPlatformAdmin(user?.role);
  const [tab, setTab] = useState<"cost" | "budget">("cost");
  const activeTab = resolveCostTab(tab, isAdmin);
  const [summary, setSummary] = useState<CostSummaryPayload | null>(null);
  const [source, setSource] = useState<CostGroupSource>("squads");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const data = await apiGet<CostSummaryPayload>(`/costs/summary?days=${WINDOW_DAYS}`, token);
      setSummary(data);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "cost summary fetch failed");
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    if (!authed) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setLoading(false);
      return;
    }
    let active = true;
    const tick = () => {
      if (active) void load();
    };
    void load();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") tick();
    }, POLL_MS);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [token, authed, load]);

  function renderTabContent() {
    if (activeTab !== "cost") {
      return isAdmin ? <AdminBudgetPanel token={token} /> : null;
    }
    if (loading && !summary) {
      return <EmptyState title="Crunching numbers…" hint="Aggregating metering across your squads." />;
    }
    return summary ? (
      <CostTabContent summary={summary} source={source} onSourceChange={setSource} />
    ) : null;
  }

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Cost Management</h1>
        {error ? <div className="notice error">{error}</div> : null}
        <div className="tabs">
          <TabButton active={activeTab === "cost"} label="Cost" onClick={() => setTab("cost")} />
          {isAdmin ? (
            <TabButton active={activeTab === "budget"} label="Budget" onClick={() => setTab("budget")} />
          ) : null}
        </div>

        {renderTabContent()}
      </AppShell>
    </AuthGate>
  );
}
