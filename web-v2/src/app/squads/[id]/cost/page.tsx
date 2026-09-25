"use client";

import { useParams } from "next/navigation";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { MetricTile } from "../../../../components/MetricTile";
import { SquadRail } from "../../../../components/SquadRail";
import { useApi } from "../../../../lib/useApi";
import { formatCost, formatTokens } from "../../../../lib/format";
import type { MeteringSummary } from "../../../../lib/api";

export default function SquadCostPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id ?? "");
  const metering = useApi<MeteringSummary>(`/squads/${squadId}/metering`, 30000);

  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName="Squad" />}>
        <h1 className="page-title">Cost</h1>
        {metering.error ? (
          <div className="notice error">{metering.error}</div>
        ) : (
          <div className="metric-grid">
            <MetricTile label="Squad spend" value={metering.loading ? "…" : formatCost(metering.data)} sub="lifetime" />
            <MetricTile label="Tokens" value={metering.loading ? "…" : formatTokens(metering.data)} sub="input · output" />
          </div>
        )}
        <p style={{ color: "var(--ink-faint)", fontSize: "var(--text-sm)" }}>
          Per-agent breakdown and budget thresholds land in UIv2-6.
        </p>
      </AppShell>
    </AuthGate>
  );
}
