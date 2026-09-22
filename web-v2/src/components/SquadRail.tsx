"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

// Contextual secondary rail for a squad. Tabs mirror the target IA
// (docs/UI-V2-PLAN.md §2).
export function SquadRail({ squadId, squadName }: { squadId: string; squadName: string }) {
  const pathname = usePathname();
  const tabs = [
    { href: `/squads/${squadId}`, label: "Overview" },
    { href: `/squads/${squadId}/board`, label: "Board" },
    { href: `/squads/${squadId}/agents`, label: "Agents" },
    { href: `/squads/${squadId}/cost`, label: "Cost" },
  ];
  return (
    <>
      <div className="rail-label">Squad</div>
      <div style={{ padding: "0 var(--space-3) var(--space-2)", fontWeight: 700, fontSize: "var(--text-lg)" }}>
        {squadName}
      </div>
      {tabs.map((tab) => {
        const active = tab.href === `/squads/${squadId}` ? pathname === tab.href : pathname.startsWith(tab.href);
        return (
          <Link key={tab.href} href={tab.href} className={active ? "nav-item active" : "nav-item"}>
            {tab.label}
          </Link>
        );
      })}
    </>
  );
}
