// Skquad UI v2 — main-menu navigation logic (S-119).
// Pure, browser-independent helpers so the menu structure can be unit-tested
// without React. AppShell wires these to useApi + expansion state.
//
// Card: Squads menu item expands to the squad names (→ squad overview);
// Agents menu item expands to agent names — of the current squad when inside
// a squad context, otherwise all accessible agents grouped by squad
// (sourced from GET /dashboard, the only scoped all-agents endpoint).

import type { Agent, Squad } from "./api";
import type { DashboardPayload } from "./dashboard";

export type MenuLink = { href: string; label: string };
export type MenuAgentGroup = { squadId: string; squadName: string; items: MenuLink[] };

export function agentHref(squadId: string, agentId: string): string {
  return `/squads/${squadId}/agents/${agentId}`;
}

// squadIdFromPath returns the squad id when the route is inside a specific
// squad (e.g. /squads/abc/board → "abc"), or "" for /squads itself and
// every non-squad route.
export function squadIdFromPath(pathname: string): string {
  const match = /^\/squads\/([^/]+)/.exec(pathname ?? "");
  return match ? match[1] : "";
}

// agentsSectionActive is true on /squads/<id>/agents and agent detail pages.
export function agentsSectionActive(pathname: string): boolean {
  return /^\/squads\/[^/]+\/agents(\/|$)/.test(pathname ?? "");
}

export function buildSquadSubitems(squads: Squad[] | null | undefined): MenuLink[] {
  return (squads ?? [])
    .map((squad) => ({ href: `/squads/${squad.id}`, label: squad.name }))
    .sort((a, b) => a.label.localeCompare(b.label));
}

// buildSquadAgentSubitems: flat, alphabetical agent links for one squad
// (the squad-context case from the card).
export function buildSquadAgentSubitems(agents: Agent[] | null | undefined): MenuLink[] {
  return (agents ?? [])
    .map((agent) => ({ href: agentHref(agent.squad_id, agent.id), label: agent.name }))
    .sort((a, b) => a.label.localeCompare(b.label));
}

// buildGlobalAgentGroups: all accessible agents (dashboard scoping),
// grouped under their squad name. Squads without agents are omitted.
export function buildGlobalAgentGroups(dashboard: DashboardPayload | null | undefined): MenuAgentGroup[] {
  return (dashboard?.squads ?? [])
    .map((squad) => ({
      squadId: squad.id,
      squadName: squad.name,
      items: (squad.agents ?? [])
        .map((agent) => ({ href: agentHref(squad.id, agent.id), label: agent.name }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    }))
    .filter((group) => group.items.length > 0)
    .sort((a, b) => a.squadName.localeCompare(b.squadName));
}

// isSubitemActive: selected-state highlighting for a sub-item link.
export function isSubitemActive(pathname: string, href: string): boolean {
  const clean = pathname ?? "";
  return clean === href || clean.startsWith(`${href}/`);
}

// effectiveExpanded: user toggle wins; otherwise the group auto-expands
// while its section is active on the current route.
export function effectiveExpanded(userToggle: boolean | undefined, routeActive: boolean): boolean {
  return userToggle ?? routeActive;
}
