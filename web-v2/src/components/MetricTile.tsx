export function MetricTile({
  label,
  value,
  sub,
  attention = false,
}: {
  label: string;
  value: string | number;
  sub?: string;
  attention?: boolean;
}) {
  return (
    <article className={attention ? "card metric attention" : "card metric"}>
      <span className="metric-label">{label}</span>
      <strong className="metric-value">{value}</strong>
      {sub ? <span className="metric-sub">{sub}</span> : null}
    </article>
  );
}
