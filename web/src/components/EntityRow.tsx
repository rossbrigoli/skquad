import Link from "next/link";
import type { ReactNode } from "react";

// One row shape for any listable entity (squad, task, agent, inbox item).
export function EntityRow({
  href,
  title,
  meta,
  side,
  trailing,
}: {
  readonly href: string;
  readonly title: string;
  readonly meta?: string;
  readonly side?: ReactNode;
  readonly trailing?: ReactNode;
}) {
  return (
    <Link href={href} className="entity-row">
      <div className="entity-main">
        <span className="entity-title">{title}</span>
        {meta ? <span className="entity-meta">{meta}</span> : null}
      </div>
      <div className="entity-side">
        {side}
        {trailing}
      </div>
    </Link>
  );
}
