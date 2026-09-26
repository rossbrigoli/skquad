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
  href: string;
  title: string;
  meta?: string;
  side?: ReactNode;
  trailing?: ReactNode;
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
