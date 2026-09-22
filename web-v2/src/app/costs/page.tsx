"use client";

import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { MetricTile } from "../../components/MetricTile";
import { useApi } from "../../lib/useApi";
import { formatCost, formatTokens } from "../../lib/format";
import type { MeteringSummary } from "../../lib/api";

export default function CostsPage() {
  const metering = useApi<MeteringSummary>("/metering/summary", 30000);

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Costs</h1>
        {metering.error ? (
          <div className="notice error">{metering.error}</div>
        ) : (
          <div className="metric-grid">
            <MetricTile label="Platform spend" value={metering.loading ? "…" : formatCost(metering.data)} sub="lifetime, all squads" />
            <MetricTile label="Tokens" value={metering.loading ? "…" : formatTokens(metering.data)} sub="input · output" />
          </div>
        )}
        <p style={{ color: "var(--ink-faint)", fontSize: "var(--text-sm)" }}>
          Per-squad/provider breakdown and budget warn/hard-stop thresholds land in UIv2-6.
        </p>
      </AppShell>
    </AuthGate>
  );
}
