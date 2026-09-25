"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";
import { useAuth } from "../../lib/auth";
import { apiGet, type Agent, type MeteringSummary, type Squad } from "../../lib/api";
import { formatCost, formatMoney, formatTokens } from "../../lib/format";

type SquadCost = {
  squad: Squad;
  total: MeteringSummary | null;
  agents: { agent: Agent; cost: MeteringSummary | null }[];
};

const POLL_MS = 60_000;

export default function CostsPage() {
  const { token, authed } = useAuth();
  const [rows, setRows] = useState<SquadCost[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!authed) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setLoading(false);
      return;
    }
    let active = true;
    const load = async () => {
      try {
        // `?? []`: a Go nil slice marshals as JSON null on empty collections;
        // guard so .map never sees null (S-121 crash class).
        const squads = (await apiGet<Squad[]>("/squads", token)) ?? [];
        const perSquad = await Promise.all(
          squads.map(async (squad) => {
            const total = await apiGet<MeteringSummary>(`/squads/${squad.id}/metering`, token).catch(() => null);
            const agents = (await apiGet<Agent[]>(`/squads/${squad.id}/agents`, token).catch(() => [] as Agent[])) ?? [];
            const agentCosts = await Promise.all(
              agents.map(async (agent) => ({
                agent,
                cost: await apiGet<MeteringSummary>(`/agents/${agent.id}/metering`, token).catch(() => null),
              })),
            );
            return { squad, total, agents: agentCosts };
          }),
        );
        if (active) {
          setRows(perSquad);
          setError("");
        }
      } catch (err) {
        if (active) {
          setError(err instanceof Error ? err.message : "cost fetch failed");
        }
      } finally {
        if (active) {
          setLoading(false);
        }
      }
    };
    load();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") load();
    }, POLL_MS);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [token, authed]);

  const grandTotal = rows.reduce((sum, row) => sum + (row.total?.cost ?? 0), 0);

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Costs</h1>
        {error ? <div className="notice error">{error}</div> : null}
        {loading && rows.length === 0 ? (
          <EmptyState title="Crunching numbers…" hint="Aggregating metering across your squads." />
        ) : rows.length === 0 ? (
          <EmptyState title="No squads" hint="Create a squad to start tracking spend." />
        ) : (
          <>
            <p style={{ color: "var(--ink-muted)", marginTop: 0 }}>
              All squads combined: <strong>{formatMoney(grandTotal)}</strong>
            </p>
            <div className="entity-list">
              {rows.map((row) => (
                <div key={row.squad.id} className="entity-row" style={{ flexDirection: "column", alignItems: "stretch" }}>
                  <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
                    <Link href={`/squads/${row.squad.id}`} className="entity-title" style={{ display: "block" }}>
                      {row.squad.name}
                    </Link>
                    <strong>{formatCost(row.total)}</strong>
                  </div>
                  <div className="entity-meta" style={{ marginTop: "var(--space-1)" }}>
                    {formatTokens(row.total)}
                  </div>
                  {row.agents.length > 0 ? (
                    <div style={{ marginTop: "var(--space-2)", borderTop: "1px solid var(--line)", paddingTop: "var(--space-2)" }}>
                      {row.agents.map(({ agent, cost }) => (
                        <div
                          key={agent.id}
                          style={{
                            display: "flex",
                            justifyContent: "space-between",
                            fontSize: "var(--text-sm)",
                            padding: "2px 0",
                          }}
                        >
                          <Link href={`/squads/${row.squad.id}/agents/${agent.id}`} style={{ color: "var(--ink)" }}>
                            {agent.name}
                          </Link>
                          <span className="mono">{formatCost(cost)}</span>
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
            <p className="entity-meta" style={{ marginTop: "var(--space-4)" }}>
              Per-provider breakdown and budget thresholds need new control-plane APIs (deferred per UI plan Phase 3).
            </p>
          </>
        )}
      </AppShell>
    </AuthGate>
  );
}
