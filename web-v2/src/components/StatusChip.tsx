import type { StatusKey } from "../lib/status";
import { statusLabels } from "../lib/status";

export function StatusChip({ status }: { status: StatusKey }) {
  return <span className={`chip chip-${status}`}>{statusLabels[status]}</span>;
}
