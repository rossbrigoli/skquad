"use client";

import { ReactNode, useId, useState } from "react";
import { initialExpanded, persistExpanded } from "../lib/collapsible";

function storage() {
  if (typeof window === "undefined" || !window.sessionStorage) return null;
  return window.sessionStorage;
}

// Collapsible: a titled section that is collapsed by default (S-118).
// Header row toggles content with a chevron; expansion state persists for
// the browser session via sessionStorage (keyed by `id`). Reusable — the
// Agent page (S-120) should use this same component.
export function Collapsible({
  id,
  title,
  children,
  initialOpen,
}: {
  id: string;
  title: ReactNode;
  children: ReactNode;
  /** Override the collapsed default (rare; session state still wins once set). */
  initialOpen?: boolean;
}) {
  const reactId = useId();
  const contentId = `collapsible-${id}-${reactId}`;
  const [expanded, setExpanded] = useState<boolean>(() =>
    initialExpanded(id, storage(), Boolean(initialOpen)),
  );

  const toggle = () => {
    setExpanded((prev) => {
      const next = !prev;
      persistExpanded(storage(), id, next);
      return next;
    });
  };

  return (
    <section className="collapsible" style={{ marginTop: "var(--space-5)" }}>
      <button
        type="button"
        className="collapsible-toggle"
        aria-expanded={expanded}
        aria-controls={contentId}
        onClick={toggle}
      >
        <span className="collapsible-title">{title}</span>
        <span className={`collapsible-chevron${expanded ? " collapsible-chevron-open" : ""}`} aria-hidden="true">
          <svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2">
            <path d="M4 6l4 4 4-4" />
          </svg>
        </span>
      </button>
      {expanded ? (
        <div id={contentId} className="collapsible-body">
          {children}
        </div>
      ) : null}
    </section>
  );
}
