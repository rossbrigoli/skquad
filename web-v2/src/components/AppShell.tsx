"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { useAuth } from "../lib/auth";
import { useAttention } from "../lib/useAttention";

const primaryNav = [
  { href: "/inbox", label: "Inbox" },
  { href: "/squads", label: "Squads" },
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

  return (
    <div className={secondary ? "shell with-secondary" : "shell"}>
      <nav className="rail" aria-label="Primary navigation">
        <Link href="/squads" className="rail-brand">
          <span className="dot" />
          skquad<span style={{ color: "var(--accent)" }}>v2</span>
        </Link>
        {primaryNav.map((item) => (
          <Link
            key={item.href}
            href={item.href}
            className={pathname.startsWith(item.href) ? "nav-item active" : "nav-item"}
          >
            {item.label}
            {item.href === "/inbox" && inboxBadge > 0 ? <span className="nav-badge">{inboxBadge}</span> : null}
          </Link>
        ))}
        <div className="rail-footer">
          {user ? (
            <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "var(--space-2)" }}>
              <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                {user.name || user.email}
              </span>
              <button type="button" className="btn" onClick={logout}>
                Sign out
              </button>
            </div>
          ) : (
            <span>not signed in</span>
          )}
        </div>
      </nav>
      {secondary ? (
        <aside className="rail" aria-label="Contextual navigation">
          {secondary}
        </aside>
      ) : null}
      <main className="content">{children}</main>
    </div>
  );
}
