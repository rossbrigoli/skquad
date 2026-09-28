// S-169: month-to-date spend chip. The control-plane metering endpoint
// accepts ?since=<RFC3339> and aggregates only events at/after that
// instant. The MTD window starts at local midnight on the first of the
// current month — "this month" means the viewer's calendar month, so the
// boundary is computed in the browser's zone and sent as UTC.
export function monthStartISO(now: Date = new Date()): string {
  const start = new Date(now.getFullYear(), now.getMonth(), 1);
  return start.toISOString();
}
