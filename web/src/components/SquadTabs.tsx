"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { IconBoard, IconContext, IconCosts, IconOverview } from "./icons";

// S-167: squad section navigation. Was a contextual second sidebar
// column (SquadRail); now a horizontal tab bar rendered at the top of
// every squad screen (Overview / Board / Squad Context / Cost).
// "Prompt" is renamed "Squad Context" per S-165 item 1/4. Routes are
// unchanged. S-172: each tab carries an icon like the parent menu items.
// S-211: the Agents tab is removed — the agent tiles and the
// "+ New agent" affordance now live on the Overview tab. The
// /squads/<id>/agents route still exists (deep links, agent detail).
export function SquadTabs({ squadId }: { squadId: string }) {
  const pathname = usePathname();
  const tabs = [
    { href: `/squads/${squadId}`, label: "Overview", exact: true, Icon: IconOverview },
    { href: `/squads/${squadId}/board`, label: "Board", exact: false, Icon: IconBoard },
    { href: `/squads/${squadId}/prompt`, label: "Squad Context", exact: false, Icon: IconContext },
    { href: `/squads/${squadId}/cost`, label: "Cost", exact: false, Icon: IconCosts },
  ];
  return (
    <nav className="squad-tabs" aria-label="Squad sections">
      {tabs.map((tab) => {
        const active = tab.exact ? pathname === tab.href : pathname.startsWith(tab.href);
        return (
          <Link
            key={tab.href}
            href={tab.href}
            className={active ? "squad-tab active" : "squad-tab"}
            aria-current={active ? "page" : undefined}
          >
            <span className="squad-tab-inner">
              <tab.Icon size={14} />
              {tab.label}
            </span>
          </Link>
        );
      })}
    </nav>
  );
}
