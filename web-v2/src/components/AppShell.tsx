"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";
import type { ReactNode } from "react";
import { useAuth } from "../lib/auth";
import { useAttention } from "../lib/useAttention";
import { useApi } from "../lib/useApi";
import type { Agent, Squad } from "../lib/api";
import type { DashboardPayload } from "../lib/dashboard";
import { ThemeToggle } from "./ThemeToggle";
import { UserMenu } from "./UserMenu";
import {
  agentsSectionActive,
  buildGlobalAgentGroups,
  buildSquadAgentSubitems,
  buildSquadSubitems,
  effectiveExpanded,
  isSubitemActive,
  squadIdFromPath,
} from "../lib/menu";

// S-119: Squads and Agents are expandable menu groups. Squads sub-items are
// the accessible squads (GET /squads, same scoping as the Squads page).
// Agents sub-items follow the same pattern: inside a squad context they are
// that squad's agents (GET /squads/<id>/agents, same source as the squad's
// Agents tab); globally they are every accessible agent grouped by squad,
// sourced from GET /dashboard (the only scoped all-agents endpoint).
const simpleNav = [
  { href: "/dashboard", label: "Dashboard" },
  { href: "/inbox", label: "Inbox" },
];
const tailNav = [
  { href: "/costs", label: "Costs" },
  { href: "/settings", label: "Settings" },
];

// Two-rail shell: persistent primary nav + optional contextual secondary rail
// (Paperclip pattern). Pages pass `secondary` for squad/agent/task context.
export function AppShell({
  children,
  secondary,
}: {
  children: ReactNode;
  secondary?: ReactNode;
}) {
  const pathname = usePathname();
  const { user, logout } = useAuth();
  const { items } = useAttention();
  const inboxBadge = items.length;
  const [menuOpen, setMenuOpen] = useState(false);
  const [lastPath, setLastPath] = useState(pathname);
  // Explicit user toggles; absent = follow the route (auto-expand when active).
  const [groupToggles, setGroupToggles] = useState<Record<string, boolean>>({});

  const squadContextId = squadIdFromPath(pathname);
  const squads = useApi<Squad[]>("/squads");
  const squadAgents = useApi<Agent[]>(squadContextId ? `/squads/${squadContextId}/agents` : "");
  const globalDashboard = useApi<DashboardPayload>(squadContextId ? "" : "/dashboard");

  const squadsActive = (pathname || "").startsWith("/squads");
  const agentsActive = agentsSectionActive(pathname);
  const squadsExpanded = effectiveExpanded(groupToggles.squads, squadsActive);
  const agentsExpanded = effectiveExpanded(groupToggles.agents, agentsActive);

  const toggleGroup = (key: string, currentlyExpanded: boolean) => {
    setGroupToggles((prev) => ({ ...prev, [key]: !currentlyExpanded }));
  };

  const squadSubitems = buildSquadSubitems(squads.data);
  const agentSubitems = squadContextId
    ? buildSquadAgentSubitems(squadAgents.data)
    : [];
  const agentGroups = squadContextId ? [] : buildGlobalAgentGroups(globalDashboard.data);
  const agentsLoading = squadContextId ? squadAgents.loading : globalDashboard.loading;

  // Navigating closes the drawer so you never land on a covered page.
  // Render-time adjustment (React's recommended pattern) avoids a cascading
  // setState inside an effect.
  if (lastPath !== pathname) {
    setLastPath(pathname);
    if (menuOpen) setMenuOpen(false);
  }

  const renderSublink = (link: { href: string; label: string }) => (
    <Link
      key={link.href}
      href={link.href}
      className={isSubitemActive(pathname, link.href) ? "nav-item nav-subitem active" : "nav-item nav-subitem"}
    >
      {link.label}
    </Link>
  );

  return (
    <div className={`${secondary ? "shell with-secondary" : "shell"}${menuOpen ? " drawer-open" : ""}`}>
      <button
        type="button"
        className="menu-button"
        aria-label={menuOpen ? "Close menu" : "Open menu"}
        aria-expanded={menuOpen}
        onClick={() => setMenuOpen((open) => !open)}
      >
        {menuOpen ? "✕" : "☰"}
      </button>
      <div className="drawer-backdrop" aria-hidden="true" onClick={() => setMenuOpen(false)} />
      {/* S-117: theme switcher pinned top-right on every page. */}
      <ThemeToggle />
      <nav className="rail rail-primary" aria-label="Primary navigation">
        <Link href="/dashboard" className="rail-brand">
          <span className="dot" />
          skquad<span style={{ color: "var(--accent)" }}>v2</span>
        </Link>
        {simpleNav.map((item) => (
          <Link
            key={item.href}
            href={item.href}
            className={pathname.startsWith(item.href) ? "nav-item active" : "nav-item"}
          >
            {item.label}
            {item.href === "/inbox" && inboxBadge > 0 ? <span className="nav-badge">{inboxBadge}</span> : null}
          </Link>
        ))}
        <div className="nav-group">
          <div className={squadsActive ? "nav-item nav-group-parent active" : "nav-item nav-group-parent"}>
            <Link href="/squads" className="nav-group-link">
              Squads
            </Link>
            <button
              type="button"
              className="nav-group-chevron"
              aria-label={squadsExpanded ? "Collapse squads" : "Expand squads"}
              aria-expanded={squadsExpanded}
              onClick={() => toggleGroup("squads", squadsExpanded)}
            >
              {squadsExpanded ? "▾" : "▸"}
            </button>
          </div>
          {squadsExpanded ? (
            <div className="nav-sub" aria-label="Squads">
              {squadSubitems.map(renderSublink)}
              {!squadSubitems.length ? (
                <div className="nav-sub-empty">{squads.loading ? "loading…" : "no squads yet"}</div>
              ) : null}
            </div>
          ) : null}
        </div>
        <div className="nav-group">
          <button
            type="button"
            className={agentsActive ? "nav-item nav-group-parent active" : "nav-item nav-group-parent"}
            aria-label={agentsExpanded ? "Collapse agents" : "Expand agents"}
            aria-expanded={agentsExpanded}
            onClick={() => toggleGroup("agents", agentsExpanded)}
          >
            <span className="nav-group-link">Agents</span>
            <span className="nav-group-chevron" aria-hidden="true">
              {agentsExpanded ? "▾" : "▸"}
            </span>
          </button>
          {agentsExpanded ? (
            <div className="nav-sub" aria-label="Agents">
              {squadContextId ? (
                agentSubitems.map(renderSublink)
              ) : (
                agentGroups.map((group) => (
                  <div key={group.squadId} className="nav-subgroup">
                    <div className="nav-subgroup-label">{group.squadName}</div>
                    {group.items.map(renderSublink)}
                  </div>
                ))
              )}
              {!agentSubitems.length && !agentGroups.length ? (
                <div className="nav-sub-empty">{agentsLoading ? "loading…" : "no agents yet"}</div>
              ) : null}
            </div>
          ) : null}
        </div>
        {tailNav.map((item) => (
          <Link
            key={item.href}
            href={item.href}
            className={pathname.startsWith(item.href) ? "nav-item active" : "nav-item"}
          >
            {item.label}
          </Link>
        ))}
        <div className="rail-footer">
          {/* S-117: name opens the profile popover (avatar, role, sign out). */}
          <UserMenu user={user} onSignOut={logout} />
        </div>
      </nav>
      {secondary ? (
        <aside className="rail rail-secondary" aria-label="Contextual navigation">
          {secondary}
        </aside>
      ) : null}
      <main className="content">{children}</main>
    </div>
  );
}
