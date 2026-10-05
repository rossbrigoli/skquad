"use client";

// S-230: AI provider tiles for the dashboard. Same tile anatomy as the
// S-211 agent tiles (icon · body · status), reusing the shared tile CSS.
// Each tile carries a green ONLINE indicator when the provider liveness
// probe succeeds, and the provider's aggregated last-30-days cost across
// all of its registered AI models (rolling window, not calendar month).

import { IconCloud } from "./icons";
import { providerChip, type DashboardProvider } from "../lib/dashboard";
import type { ProviderUsage } from "../lib/usage";
import { formatCompact, formatMoney } from "../lib/format";

// providerTileClass maps liveness to the tile modifier class. Exported
// so the class contract is unit-testable without rendering.
export function providerTileClass(online: boolean): string {
  const parts = ["provider-tile"];
  if (online) parts.push("provider-tile--online");
  return parts.join(" ");
}

// onlineIndicatorLabel is the tile's liveness badge text.
export function onlineIndicatorLabel(online: boolean): string {
  return online ? "ONLINE" : "OFFLINE";
}

export function ProviderTile({
  provider,
  usageRow,
  currency,
}: {
  readonly provider: DashboardProvider;
  readonly usageRow: ProviderUsage | undefined;
  readonly currency: string;
}) {
  const chip = providerChip(provider);
  const inactive = (provider.status ?? "").toLowerCase() !== "active";
  return (
    <div className={providerTileClass(provider.online && !inactive)} aria-label={`${provider.name} — ${chip.label}`}>
      <span className="agent-tile-icon" aria-hidden="true">
        <IconCloud size={28} />
      </span>
      <span className="agent-tile-body">
        <span className="agent-tile-name">{provider.name}</span>
        <span className="agent-tile-role">
          {provider.kind}
          {provider.latency_ms ? ` · ${provider.latency_ms} ms` : ""}
          {provider.error ? ` · ${provider.error}` : ""}
        </span>
        <span className="agent-tile-cost mono">
          {usageRow
            ? `last 30 days ${formatMoney(usageRow.cost, currency)} · ${formatCompact(usageRow.tokens)} tokens`
            : "no usage in the last 30 days"}
        </span>
      </span>
      <span className="agent-tile-status">
        <span className={provider.online && !inactive ? "provider-online" : "provider-offline"}>
          <span className="provider-online-dot" aria-hidden="true" />
          {onlineIndicatorLabel(provider.online && !inactive)}
        </span>
      </span>
      {(usageRow?.models?.length ?? 0) > 0 ? (
        <div className="provider-models provider-tile-models">
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
}

export function ProviderTilesGrid({
  providers,
  usageMap,
  currency,
}: {
  readonly providers: readonly DashboardProvider[];
  readonly usageMap: ReadonlyMap<string, ProviderUsage>;
  readonly currency: string;
}) {
  return (
    <div className="agent-tile-grid">
      {providers.map((provider) => (
        <ProviderTile
          key={provider.id}
          provider={provider}
          usageRow={usageMap.get(provider.id)}
          currency={currency}
        />
      ))}
    </div>
  );
}
