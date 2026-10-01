// Skquad UI v2 — dashboard usage (S-190) pure logic.
// Mirrors GET /api/v1/dashboard/usage and folds the server's daily series
// into a stacked-bar chart model. All math lives here so it is unit-testable
// without React; BarChart.tsx only renders this model.

export type UsagePoint = {
  day: string;
  input_tokens: number;
  output_tokens: number;
  tokens: number;
  cost: number;
};

export type UsageSeries = {
  id: string;
  name: string;
  squad_id?: string;
  squad_name?: string;
  points: UsagePoint[];
};

export type ProviderModelUsage = {
  model: string;
  tokens: number;
  cost: number;
};

export type ProviderUsage = {
  provider_id: string;
  provider_name: string;
  tokens: number;
  cost: number;
  models: ProviderModelUsage[];
};

export type PlatformUsage = {
  total_cost: number;
  mtd_cost: number;
  users: number;
  agents: number;
};

export type DashboardUsagePayload = {
  scope: string;
  days: string[];
  mtd_start: string;
  currency?: string;
  squad_mtd_cost: number;
  by_squad: UsageSeries[];
  by_agent: UsageSeries[];
  providers: ProviderUsage[];
  platform?: PlatformUsage | null;
};

export type ChartMode = "tokens" | "cost";

// Deterministic palette: series index → color, so a squad/agent keeps the
// same color across polls and between the two histograms' legends within a
// chart.
export const CHART_PALETTE = [
  "#6366f1",
  "#f59e0b",
  "#10b981",
  "#ef4444",
  "#0ea5e9",
  "#a855f7",
  "#14b8a6",
  "#f97316",
  "#84cc16",
  "#e11d48",
  "#8b5cf6",
  "#22c55e",
];

export function paletteColor(index: number): string {
  return CHART_PALETTE[index % CHART_PALETTE.length];
}

export function pointValue(point: UsagePoint | undefined, mode: ChartMode): number {
  if (!point) return 0;
  return mode === "cost" ? (point.cost ?? 0) : (point.tokens ?? 0);
}

export type ChartSegment = {
  name: string;
  color: string;
  value: number;
  offset: number;
};

export type ChartColumn = {
  day: string;
  total: number;
  segments: ChartSegment[];
};

export type ChartModel = {
  columns: ChartColumn[];
  maxTotal: number;
  legend: { name: string; color: string }[];
  hasData: boolean;
};

// buildStackedChart folds aligned series into per-day stacked columns.
// `days` is the server-provided axis; each series' points are expected to
// align 1:1 with it (the control-plane zero-fills), but we index by day
// string defensively so a misaligned payload degrades to zeros, not
// wrong bars.
export function buildStackedChart(days: string[], series: UsageSeries[], mode: ChartMode): ChartModel {
  const legend = series.map((s, i) => ({ name: s.name || s.id, color: paletteColor(i) }));
  const columns: ChartColumn[] = [];
  let maxTotal = 0;
  let hasData = false;

  const byDayPerSeries = series.map((s) => {
    const map = new Map<string, UsagePoint>();
    for (const p of s.points ?? []) map.set(p.day, p);
    return map;
  });

  for (const day of days) {
    const segments: ChartSegment[] = [];
    let total = 0;
    series.forEach((s, i) => {
      const value = pointValue(byDayPerSeries[i].get(day), mode);
      if (value <= 0) return;
      segments.push({ name: s.name || s.id, color: legend[i].color, value, offset: total });
      total += value;
    });
    if (total > 0) hasData = true;
    maxTotal = Math.max(maxTotal, total);
    columns.push({ day, total, segments });
  }

  return { columns, maxTotal, legend, hasData };
}

// providerUsageByIndex maps provider_id → usage so the dashboard can
// annotate its existing provider list without re-finding entries.
export function providerUsageMap(providers: ProviderUsage[] | undefined): Map<string, ProviderUsage> {
  const map = new Map<string, ProviderUsage>();
  for (const p of providers ?? []) map.set(p.provider_id, p);
  return map;
}
