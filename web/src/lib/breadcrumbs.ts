// Skquad UI v2 — breadcrumb derivation for the S-127 top nav bar.
//
// Pure, browser-independent helper: maps a pathname to a list of crumbs so
// the top bar can render "Squads / <squad> / <agent>" without touching
// React. Dynamic segments ([id], [agentId], [tid]) are resolved from
// caller-supplied hints (squad/agent names already loaded by AppShell);
// when a hint is unavailable the crumb falls back to a generic label
// ("Squad", "Agent", "Task") or a humanised static segment.

export type Crumb = { readonly label: string; readonly href: string | null };

export type BreadcrumbHints = {
  readonly squadName?: string;
  readonly agentName?: string;
  readonly taskName?: string;
};

// Static, well-known route segments and their display labels.
const STATIC_LABELS: Record<string, string> = {
  dashboard: "Dashboard",
  inbox: "Inbox",
  costs: "Costs",
  settings: "Settings",
  squads: "Squads",
  agents: "Agents",
  board: "Board",
  tasks: "Tasks",
  cost: "Cost",
  models: "AI Models",
  appearance: "Appearance",
  profile: "Profile",
};

// Dynamic segments live under these parents; each parent selects the hint
// (and generic fallback) used for the segment that follows it.
const HINT_BY_PARENT: Record<string, keyof BreadcrumbHints> = {
  squads: "squadName",
  agents: "agentName",
  tasks: "taskName",
};

const GENERIC_BY_PARENT: Record<string, string> = {
  squads: "Squad",
  agents: "Agent",
  tasks: "Task",
};

export function agentIdFromPath(pathname: string): string {
  const match = /^\/squads\/[^/]+\/agents\/([^/]+)/.exec(pathname ?? "");
  return match ? match[1] : "";
}

export function taskIdFromPath(pathname: string): string {
  const match = /^\/squads\/[^/]+\/tasks\/([^/]+)/.exec(pathname ?? "");
  return match ? match[1] : "";
}

function humanize(segment: string): string {
  const spaced = segment.replace(/[-_]+/g, " ").trim();
  if (!spaced) return segment;
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

// breadcrumbsForPath returns the crumb trail for a pathname. Every crumb
// except the last is linked (href set); the last crumb is plain text
// (href null). Root "/" renders as a single "Dashboard" crumb.
export function breadcrumbsForPath(pathname: string, hints: BreadcrumbHints = {}): Crumb[] {
  const segments = (pathname ?? "").split("/").filter(Boolean);
  if (segments.length === 0) {
    return [{ label: "Dashboard", href: null }];
  }

  const crumbs: Crumb[] = [];
  let path = "";
  let parent = "";
  for (let i = 0; i < segments.length; i += 1) {
    const segment = segments[i];
    path += `/${segment}`;
    const isLast = i === segments.length - 1;

    let label: string;
    const staticLabel = STATIC_LABELS[segment];
    if (staticLabel) {
      label = staticLabel;
    } else {
      const hintKey = HINT_BY_PARENT[parent];
      const hinted = hintKey ? hints[hintKey] : undefined;
      label = hinted || GENERIC_BY_PARENT[parent] || humanize(segment);
    }

    crumbs.push({ label, href: isLast ? null : path });
    parent = segment;
  }
  return crumbs;
}
