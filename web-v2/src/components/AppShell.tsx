"use client";

import Image from "next/image";
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

type NavItem = { href: string; label: string };

function NavSubLink({ pathname, link }: { readonly pathname: string; readonly link: NavItem }) {
  return (
    <Link
      href={link.href}
      className={isSubitemActive(pathname, link.href) ? "nav-item nav-subitem active" : "nav-item nav-subitem"}
    >
      {link.label}
    </Link>
  );
}

function SquadsNavGroup({
  pathname,
  active,
  expanded,
  items,
  loading,
  onToggle,
}: {
  readonly pathname: string;
  readonly active: boolean;
  readonly expanded: boolean;
  readonly items: NavItem[];
  readonly loading: boolean;
  readonly onToggle: () => void;
}) {
  return (
    <div className="nav-group">
      <div className={active ? "nav-item nav-group-parent active" : "nav-item nav-group-parent"}>
        <Link href="/squads" className="nav-group-link">
          Squads
        </Link>
        <button
          type="button"
          className="nav-group-chevron"
          aria-label={expanded ? "Collapse squads" : "Expand squads"}
          aria-expanded={expanded}
          onClick={onToggle}
        >
          {expanded ? "▾" : "▸"}
        </button>
      </div>
      {expanded ? (
        <div className="nav-sub" aria-label="Squads">
          {items.map((link) => (
            <NavSubLink key={link.href} pathname={pathname} link={link} />
          ))}
          {items.length === 0 ? (
            <div className="nav-sub-empty">{loading ? "loading…" : "no squads yet"}</div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function AgentNavItems({
  pathname,
  squadContextId,
  agentSubitems,
  agentGroups,
}: {
  readonly pathname: string;
  readonly squadContextId: string;
  readonly agentSubitems: NavItem[];
  readonly agentGroups: ReturnType<typeof buildGlobalAgentGroups>;
}) {
  if (squadContextId) {
    return agentSubitems.map((link) => <NavSubLink key={link.href} pathname={pathname} link={link} />);
  }
  return agentGroups.map((group) => (
    <div key={group.squadId} className="nav-subgroup">
      <div className="nav-subgroup-label">{group.squadName}</div>
      {group.items.map((link) => (
        <NavSubLink key={link.href} pathname={pathname} link={link} />
      ))}
    </div>
  ));
}

function AgentsNavGroup({
  pathname,
  active,
  expanded,
  squadContextId,
  agentSubitems,
  agentGroups,
  loading,
  onToggle,
}: {
  readonly pathname: string;
  readonly active: boolean;
  readonly expanded: boolean;
  readonly squadContextId: string;
  readonly agentSubitems: NavItem[];
  readonly agentGroups: ReturnType<typeof buildGlobalAgentGroups>;
  readonly loading: boolean;
  readonly onToggle: () => void;
}) {
  return (
    <div className="nav-group">
      <button
        type="button"
        className={active ? "nav-item nav-group-parent active" : "nav-item nav-group-parent"}
        aria-label={expanded ? "Collapse agents" : "Expand agents"}
        aria-expanded={expanded}
        onClick={onToggle}
      >
        <span className="nav-group-link">Agents</span>
        <span className="nav-group-chevron" aria-hidden="true">
          {expanded ? "▾" : "▸"}
        </span>
      </button>
      {expanded ? (
        <div className="nav-sub" aria-label="Agents">
          <AgentNavItems
            pathname={pathname}
            squadContextId={squadContextId}
            agentSubitems={agentSubitems}
            agentGroups={agentGroups}
          />
          {agentSubitems.length === 0 && agentGroups.length === 0 ? (
            <div className="nav-sub-empty">{loading ? "loading…" : "no agents yet"}</div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function PrimaryRail({
  pathname,
  user,
  logout,
  inboxBadge,
  squadsActive,
  squadsExpanded,
  squadSubitems,
  squadsLoading,
  agentsActive,
  agentsExpanded,
  squadContextId,
  agentSubitems,
  agentGroups,
  agentsLoading,
  onToggleGroup,
}: {
  readonly pathname: string;
  readonly user: ReturnType<typeof useAuth>["user"];
  readonly logout: ReturnType<typeof useAuth>["logout"];
  readonly inboxBadge: number;
  readonly squadsActive: boolean;
  readonly squadsExpanded: boolean;
  readonly squadSubitems: NavItem[];
  readonly squadsLoading: boolean;
  readonly agentsActive: boolean;
  readonly agentsExpanded: boolean;
  readonly squadContextId: string;
  readonly agentSubitems: NavItem[];
  readonly agentGroups: ReturnType<typeof buildGlobalAgentGroups>;
  readonly agentsLoading: boolean;
  readonly onToggleGroup: (key: string, currentlyExpanded: boolean) => void;
}) {
  return (
    <nav className="rail rail-primary" aria-label="Primary navigation">
      <Link href="/dashboard" className="rail-brand" aria-label="Skquad dashboard">
        <Image src="/skquad-logo-64.png" width={28} height={28} alt="" className="rail-brand-logo" priority />
        skquad
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
      <SquadsNavGroup
        pathname={pathname}
        active={squadsActive}
        expanded={squadsExpanded}
        items={squadSubitems}
        loading={squadsLoading}
        onToggle={() => onToggleGroup("squads", squadsExpanded)}
      />
      <AgentsNavGroup
        pathname={pathname}
        active={agentsActive}
        expanded={agentsExpanded}
        squadContextId={squadContextId}
        agentSubitems={agentSubitems}
        agentGroups={agentGroups}
        loading={agentsLoading}
        onToggle={() => onToggleGroup("agents", agentsExpanded)}
      />
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
        <UserMenu user={user} onSignOut={logout} />
      </div>
    </nav>
  );
}

// Two-rail shell: persistent primary nav + optional contextual secondary rail
// (Paperclip pattern). Pages pass `secondary` for squad/agent/task context.
export function AppShell({
  children,
  secondary,
}: {
  readonly children: ReactNode;
  readonly secondary?: ReactNode;
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

  const squadsActive = (pathname ?? "").startsWith("/squads");
  const agentsActive = agentsSectionActive(pathname);
  const squadsExpanded = effectiveExpanded(groupToggles.squads, squadsActive);
  const agentsExpanded = effectiveExpanded(groupToggles.agents, agentsActive);

  const toggleGroup = (key: string, currentlyExpanded: boolean) => {
    setGroupToggles((prev) => ({ ...prev, [key]: !currentlyExpanded }));
  };

  const squadSubitems = buildSquadSubitems(squads.data);
  const agentSubitems = squadContextId ? buildSquadAgentSubitems(squadAgents.data) : [];
  const agentGroups = squadContextId ? [] : buildGlobalAgentGroups(globalDashboard.data);
  const agentsLoading = squadContextId ? squadAgents.loading : globalDashboard.loading;

  // Navigating closes the drawer so you never land on a covered page.
  // Render-time adjustment (React's recommended pattern) avoids a cascading
  // setState inside an effect.
  if (lastPath !== pathname) {
    setLastPath(pathname);
    if (menuOpen) setMenuOpen(false);
  }

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
      <PrimaryRail
        pathname={pathname}
        user={user}
        logout={logout}
        inboxBadge={inboxBadge}
        squadsActive={squadsActive}
        squadsExpanded={squadsExpanded}
        squadSubitems={squadSubitems}
        squadsLoading={squads.loading}
        agentsActive={agentsActive}
        agentsExpanded={agentsExpanded}
        squadContextId={squadContextId}
        agentSubitems={agentSubitems}
        agentGroups={agentGroups}
        agentsLoading={agentsLoading}
        onToggleGroup={toggleGroup}
      />
      {secondary ? (
        <aside className="rail rail-secondary" aria-label="Contextual navigation">
          {secondary}
        </aside>
      ) : null}
      <main className="content">{children}</main>
    </div>
  );
}
