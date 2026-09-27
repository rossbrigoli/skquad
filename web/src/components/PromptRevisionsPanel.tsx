"use client";

import { useState } from "react";
import { EmptyState } from "./EmptyState";
import { useApi } from "../lib/useApi";
import { formatRelativeTime } from "../lib/format";
import { tierBadge, type PromptRevision } from "../lib/prompt";

// PromptRevisionsPanel: append-only history for one scope
// (GET /prompt/revisions?scope=&scope_id=). Rows show saved_at,
// saved_by and the token count; content is expandable. 'Restore' does
// NOT hit a rollback endpoint — it prefills the parent editor with the
// revision content, and the normal save path appends a new revision
// (append-only history, resolved decision Q5).
export function PromptRevisionsPanel({
  scope,
  scopeId,
  onRestore,
}: {
  readonly scope: string;
  readonly scopeId?: string;
  readonly onRestore: (content: string, rev: PromptRevision) => void;
}) {
  const revisions = useApi<{ revisions: PromptRevision[] }>(
    `/prompt/revisions?scope=${encodeURIComponent(scope)}&scope_id=${encodeURIComponent(scopeId ?? "")}`,
    0,
  );
  const [expanded, setExpanded] = useState<string | null>(null);
  const items = revisions.data?.revisions || [];
  const badge = tierBadge(scope);

  if (revisions.loading) {
    return <div className="notice">Loading revisions…</div>;
  }
  if (revisions.error) {
    return <div className="notice error">Could not load revisions: {revisions.error}</div>;
  }
  if (items.length === 0) {
    return <EmptyState title="No saved revisions yet" hint="Revisions appear here after the first save of this prompt." />;
  }

  return (
    <div className="entity-list prompt-revisions">
      {items.map((rev) => {
        const isOpen = expanded === rev.id;
        return (
          <div key={rev.id} className="entity-row prompt-revision-row">
            <div className="entity-main">
              <span className="entity-title">
                <span className={`tier-badge ${badge.className}`}>{badge.label}</span>
                <span className="mono"> · {rev.tokens.toLocaleString("en-US")} tokens</span>
                <span className="mono" title={rev.sha256}> · {rev.sha256.slice(0, 12)}…</span>
              </span>
              <span className="entity-meta">
                saved {formatRelativeTime(rev.saved_at)} by {rev.saved_by || "unknown"}
              </span>
            </div>
            <div className="entity-side">
              <button
                type="button"
                className="btn btn-sm"
                aria-expanded={isOpen}
                onClick={() => {
                  setExpanded(isOpen ? null : rev.id);
                }}
              >
                {isOpen ? "Hide" : "View"}
              </button>
              <button
                type="button"
                className="btn btn-sm btn-primary"
                onClick={() => {
                  onRestore(rev.content, rev);
                }}
              >
                Restore
              </button>
            </div>
            {isOpen ? <pre className="prompt-tier-content prompt-revision-content">{rev.content || "(empty)"}</pre> : null}
          </div>
        );
      })}
    </div>
  );
}
