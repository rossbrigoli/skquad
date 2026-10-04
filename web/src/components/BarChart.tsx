"use client";

// S-190: lightweight SVG stacked-bar histogram. No chart dependency —
// the repo ships no chart library, so the dashboard draws its own bars
// from the ChartModel built in lib/usage.ts.
//
// S-201: the axis labels used to live inside the SVG viewBox, which is
// stretched to the container width (preserveAspectRatio="none"), so the
// text scaled up with the chart and looked oversized next to the page's
// 11–13px type. The labels now render as HTML: the x-axis is a flex row
// of equal-width cells (one per day, aligned with the SVG columns) and the
// y-axis values are absolutely positioned over the plot. The SVG keeps a
// fixed pixel height so vertical viewBox units map 1:1 to CSS pixels.

import { formatDayLabel, type ChartModel } from "../lib/usage";

const WIDTH = 760;
// PAD_TOP/PAD_BOTTOM leave room for the HTML y-axis labels that sit just
// below each gridline (vertical units map 1:1 to CSS px here).
const PAD_TOP = 14;
const PAD_BOTTOM = 18;

export function BarChart({
  model,
  formatValue,
  height = 240,
}: {
  readonly model: ChartModel;
  readonly formatValue: (n: number) => string;
  readonly height?: number;
}) {
  const plotH = height - PAD_TOP - PAD_BOTTOM;
  const count = Math.max(model.columns.length, 1);
  const colW = WIDTH / count;
  const barW = Math.max(2, colW - 3);
  const max = model.maxTotal > 0 ? model.maxTotal : 1;
  const labelEvery = Math.max(1, Math.ceil(count / 8));
  const yFor = (value: number) => PAD_TOP + plotH - (value / max) * plotH;

  if (!model.hasData) {
    return <div className="chart-empty">No usage recorded in this window.</div>;
  }

  return (
    <div className="chart-wrap">
      <div className="chart-frame" style={{ height }}>
        <svg
          viewBox={`0 0 ${WIDTH} ${height}`}
          preserveAspectRatio="none"
          className="chart-svg"
        >
          <title>Daily usage histogram</title>
          {[0, 0.5, 1].map((frac) => {
            const y = yFor(frac * max);
            return (
              <line key={frac} x1={0} y1={y} x2={WIDTH} y2={y} stroke="var(--line)" strokeWidth={1} />
            );
          })}
          {model.columns.map((column, i) => {
            const x = i * colW + (colW - barW) / 2;
            return (
              <g key={column.day}>
                {column.segments.map((segment) => (
                  <rect
                    key={`${column.day}-${segment.name}`}
                    x={x}
                    y={yFor(segment.offset + segment.value)}
                    width={barW}
                    height={Math.max(1, (segment.value / max) * plotH)}
                    fill={segment.color}
                  >
                    <title>{`${column.day} · ${segment.name}: ${formatValue(segment.value)}`}</title>
                  </rect>
                ))}
              </g>
            );
          })}
        </svg>
        <div className="chart-yaxis" aria-hidden="true">
          {[1, 0.5, 0].map((frac) => (
            <span key={frac} className="chart-y-label" style={{ top: yFor(frac * max) + 3 }}>
              {formatValue(frac * max)}
            </span>
          ))}
        </div>
      </div>
      <div className="chart-xaxis" aria-hidden="true">
        {model.columns.map((column, i) => (
          <span key={column.day} className="chart-xlabel">
            {i % labelEvery === 0 ? formatDayLabel(column.day) : ""}
          </span>
        ))}
      </div>
      <div className="chart-legend">
        {model.legend.map((entry) => (
          <span key={`${entry.name}-${entry.color}`} className="chart-legend-item">
            <span className="chart-swatch" style={{ background: entry.color }} />
            {entry.name}
          </span>
        ))}
      </div>
    </div>
  );
}
