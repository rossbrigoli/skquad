"use client";

import { Collapsible } from "./Collapsible";
import { EmptyState } from "./EmptyState";
import { TokenMeter } from "./TokenMeter";
import { useApi } from "../lib/useApi";
import { sortTiersForDisplay, tierBadge, type EffectivePrompt } from "../lib/prompt";

// EffectivePromptPanel: read-only preview of the composed prompt for one
// agent (GET /prompt/effective?agent_id=). The four tier blocks are
// stacked in trust order with badges, per-tier token counts vs caps, the
// composed total + sha256, and any composition warnings inline.
export function EffectivePromptPanel({
  agentId,
}: {
  readonly agentId: string;
}) {
  const effective = useApi<EffectivePrompt>(`/prompt/effective?agent_id=${encodeURIComponent(agentId)}`, 0);
  const tiers = effective.data ? sortTiersForDisplay(effective.data.tiers || []) : [];

  if (effective.loading) {
    return <div className="notice">Loading effective prompt…</div>;
  }
  if (effective.error) {
    return <div className="notice error">Could not load effective prompt: {effective.error}</div>;
  }
  if (!effective.data || tiers.length === 0) {
    return <EmptyState title="No prompt tiers" hint="The composer returned no tiers for this agent." />;
  }

  return (
    <div className="effective-prompt">
      <div className="effective-prompt-summary entity-meta">
        Composed total: <strong className="mono">{effective.data.total_tokens.toLocaleString("en-US")}</strong> tokens
        {" · "}sha256 <span className="mono" title={effective.data.sha256}>{effective.data.sha256.slice(0, 16)}…</span>
      </div>
      {effective.data.warnings?.map((w) => (
        <output key={w} className="notice warn" style={{ display: "block" }}>
          {w}
        </output>
      ))}
      {tiers.map((tier) => {
        const badge = tierBadge(tier.name);
        return (
          <div key={tier.name} className={`prompt-tier-block ${badge.className}`}>
            <Collapsible
              id={`effective-tier-${tier.name}-${agentId}`}
              title={
                <span className="prompt-tier-title">
                  <span className={`tier-badge ${badge.className}`}>{badge.label}</span>
                  <span className="tier-trust">{badge.trust}</span>
                  <span className="mono tier-tokens">
                    {tier.tokens.toLocaleString("en-US")} / {tier.hard_cap.toLocaleString("en-US")}
                  </span>
                </span>
              }
            >
              <TokenMeter tokens={tier.tokens} softWarn={tier.soft_warn} hardCap={tier.hard_cap} />
              <pre className="prompt-tier-content">{tier.content ? tier.content : "(empty)"}</pre>
            </Collapsible>
          </div>
        );
      })}
    </div>
  );
}
