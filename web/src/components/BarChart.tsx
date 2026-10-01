"use client";

// S-190: lightweight SVG stacked-bar histogram. No chart dependency —
// the repo ships no chart library, so the dashboard draws its own bars
// from the ChartModel built in lib/usage.ts.

import type { ChartModel } from "../lib/usage";

const WIDTH = 760;
const PAD_X = 8;
const PAD_TOP = 18;
const PAD_BOTTOM = 24;

export function BarChart({
  model,
  formatValue,
  height = 240,
}: {
  model: ChartModel;
  formatValue: (n: number) => string;
  height?: number;
}) {
  const plotW = WIDTH - PAD_X * 2;
  const plotH = height - PAD_TOP - PAD_BOTTOM;
  const count = Math.max(model.columns.length, 1);
  const colW = plotW / count;
  const barW = Math.max(2, colW - 3);
  const max = model.maxTotal > 0 ? model.maxTotal : 1;
  const labelEvery = Math.max(1, Math.ceil(count / 8));
  const yFor = (value: number) => PAD_TOP + plotH - (value / max) * plotH;

  if (!model.hasData) {
    return <div className="chart-empty">No usage recorded in this window.</div>;
  }

  return (
    <div className="chart-wrap">
      <svg
        viewBox={`0 0 ${WIDTH} ${height}`}
        role="img"
        aria-label="Daily usage histogram"
        preserveAspectRatio="none"
        style={{ width: "100%", height: "auto", display: "block" }}
      >
        {[0, 0.5, 1].map((frac) => {
          const y = yFor(frac * max);
          return (
            <g key={frac}>
              <line x1={PAD_X} y1={y} x2={WIDTH - PAD_X} y2={y} stroke="var(--line)" strokeWidth={1} />
              <text x={PAD_X} y={y - 4} fontSize={10} fill="var(--ink-faint)">
                {formatValue(frac * max)}
              </text>
            </g>
          );
        })}
        {model.columns.map((column, i) => {
          const x = PAD_X + i * colW + (colW - barW) / 2;
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
              {i % labelEvery === 0 ? (
                <text
                  x={x + barW / 2}
                  y={height - 8}
                  fontSize={10}
                  fill="var(--ink-faint)"
                  textAnchor="middle"
                >
                  {column.day.slice(5)}
                </text>
              ) : null}
            </g>
          );
        })}
      </svg>
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
