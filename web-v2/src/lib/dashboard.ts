// Skquad UI v2 — dashboard (S-116) pure logic.
// The page renders GET /api/v1/dashboard; every derived value lives here so
// it can be unit-tested without React.

import type { MeteringSummary } from "./api";
import { agentStatus } from "./status";

export type DashboardAgent = {
  id: string;
  squad_id: string;
  name: string;
  role?: string;
  status: string;
  cost?: MeteringSummary | null;
};

export type DashboardSquad = {
  id: string;
  name: string;
  status?: string;
  owner_id?: string;
  owner_name?: string;
  task_counts?: Record<string, number>;
  cost?: MeteringSummary | null;
  agents?: DashboardAgent[];
};

export type DashboardProvider = {
  id: string;
  name: string;
  kind?: string;
  base_url?: string;
  status: string;
  online: boolean;
  latency_ms?: number;
  error?: string;
};

export type DashboardResource = {
  id: string;
  type: string;
  name: string;
  status: string;
};

export type DashboardPayload = {
  scope: "all" | "personal" | string;
  squads: DashboardSquad[];
  providers: DashboardProvider[];
  resources: DashboardResource[];
};

export function taskCount(squad: DashboardSquad, status: string): number {
  return squad.task_counts?.[status] ?? 0;
}

// providerChip maps a provider to a chip. Lifecycle beats liveness: a
// deprecated provider renders "inactive", never a scary red offline.
export function providerChip(provider: DashboardProvider): { label: string; className: string } {
  if ((provider.status || "").toLowerCase() !== "active") {
    return { label: "inactive", className: "chip chip-paused" };
  }
  return provider.online
    ? { label: "online", className: "chip chip-running" }
    : { label: "offline", className: "chip chip-error" };
}

export function resourceChip(status: string): { label: string; className: string } {
  const normalized = (status || "").toLowerCase();
  if (normalized === "active") {
    return { label: "active", className: "chip chip-done" };
  }
  if (normalized === "deprecated") {
    return { label: "deprecated", className: "chip chip-paused" };
  }
  return { label: normalized || "unknown", className: "chip chip-idle" };
}

export type DashboardTotals = {
  squads: number;
  agents: number;
  agentsRunning: number;
  agentsIdle: number;
  agentsError: number;
  totalCost: number;
  currency: string;
};

export function dashboardTotals(payload: DashboardPayload | null): DashboardTotals {
  const totals: DashboardTotals = {
    squads: 0,
    agents: 0,
    agentsRunning: 0,
    agentsIdle: 0,
    agentsError: 0,
    totalCost: 0,
    currency: "USD",
  };
  if (!payload) {
    return totals;
  }
  totals.squads = payload.squads?.length ?? 0;
  for (const squad of payload.squads ?? []) {
    totals.totalCost += squad.cost?.cost ?? 0;
    if (squad.cost?.currency) {
      totals.currency = squad.cost.currency;
    }
    for (const agent of squad.agents ?? []) {
      totals.agents += 1;
      const status = agentStatus({ id: agent.id, squad_id: agent.squad_id, name: agent.name, status: agent.status });
      if (status === "error") totals.agentsError += 1;
      else if (status === "running") totals.agentsRunning += 1;
      else totals.agentsIdle += 1;
    }
  }
  return totals;
}
