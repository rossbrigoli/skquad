"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

// S-167: squad section navigation. Was a contextual second sidebar
// column (SquadRail); now a horizontal tab bar rendered at the top of
// every squad screen (Overview / Board / Agents / Squad Context / Cost).
// "Prompt" is renamed "Squad Context" per S-165 item 1/4. Routes are
// unchanged.
export function SquadTabs({ squadId }: { squadId: string }) {
  const pathname = usePathname();
  const tabs = [
    { href: `/squads/${squadId}`, label: "Overview", exact: true },
    { href: `/squads/${squadId}/board`, label: "Board", exact: false },
    { href: `/squads/${squadId}/agents`, label: "Agents", exact: false },
    { href: `/squads/${squadId}/prompt`, label: "Squad Context", exact: false },
    { href: `/squads/${squadId}/cost`, label: "Cost", exact: false },
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
            {tab.label}
          </Link>
        );
      })}
    </nav>
  );
}
