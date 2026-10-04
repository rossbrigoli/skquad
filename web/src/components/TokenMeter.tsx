"use client";

import { meterLevel, meterPercent, type TokenMeterLevel } from "../lib/prompt";

// TokenMeter: horizontal bar for one prompt tier. Caps come from the API
// (soft_warn / hard_cap) — never hardcoded in the UI. Level semantics
// match the control-plane: over only when the cap is exceeded.
export function TokenMeter({
  tokens,
  softWarn,
  hardCap,
  levelOverride,
}: {
  readonly tokens: number;
  readonly softWarn: number;
  readonly hardCap: number;
  /** e.g. "checking" while a debounced validate is in flight. */
  readonly levelOverride?: "checking" | null;
}) {
  const level: TokenMeterLevel = meterLevel(tokens, softWarn, hardCap);
  const pct = meterPercent(tokens, hardCap);
  const softPct = hardCap > 0 ? Math.min(100, Math.round((softWarn / hardCap) * 100)) : 0;
  const checking = levelOverride === "checking";
  return (
    <div className={`token-meter meter-${level}${checking ? " meter-checking" : ""}`}>
      {/* S-189/S6819: native <meter> for assistive tech; the custom bar below is purely visual. */}
      <meter
        className="token-meter-native"
        style={{ position: "absolute", width: "1px", height: "1px", overflow: "hidden", clipPath: "inset(50%)" }}
        min={0}
        max={hardCap}
        value={tokens}
        aria-label={`${tokens} tokens, soft limit ${softWarn}, hard cap ${hardCap}`}
      >
        {`${pct}%`}
      </meter>
      <div className="token-meter-track" aria-hidden="true">
        <div className="token-meter-fill" style={{ width: `${pct}%` }} />
        {softPct > 0 && softPct < 100 ? (
          <div className="token-meter-soft-mark" style={{ left: `${softPct}%` }} title={`soft limit: ${softWarn}`} />
        ) : null}
      </div>
      <span className="token-meter-label mono">
        {tokens.toLocaleString("en-US")} / {hardCap.toLocaleString("en-US")} tokens
        {level === "warn" ? <span className="token-meter-flag"> · over soft limit {softWarn.toLocaleString("en-US")}</span> : null}
        {level === "over" ? <span className="token-meter-flag"> · over hard cap</span> : null}
        {checking ? <span className="token-meter-flag"> · checking…</span> : null}
      </span>
    </div>
  );
}
