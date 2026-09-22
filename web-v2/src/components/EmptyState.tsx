import type { ReactNode } from "react";

// Empty states say what to do first — never just "No data".
export function EmptyState({
  title,
  hint,
  action,
}: {
  title: string;
  hint?: string;
  action?: ReactNode;
}) {
  return (
    <div className="empty-state">
      <span className="empty-title">{title}</span>
      {hint ? <span>{hint}</span> : null}
      {action ? <div>{action}</div> : null}
    </div>
  );
}
