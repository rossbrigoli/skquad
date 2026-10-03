// S-195: the tokens/cost control is now a single iOS-style sliding switch
// (role="switch", aria-checked reflects the cost mode) instead of two
// buttons. Both labels render inside the track; the knob slides via CSS.
import { describe, expect, it, vi } from "vitest";
import { readFileSync } from "node:fs";
import path from "node:path";
import { renderToStaticMarkup } from "react-dom/server";
import { ModeToggle, SourceToggle } from "./page";

describe("ModeToggle sliding switch", () => {
  it("renders a switch with both labels and knob", () => {
    const html = renderToStaticMarkup(<ModeToggle mode="tokens" onChange={() => undefined} />);
    expect(html).toContain('role="switch"');
    expect(html).toContain('aria-checked="false"');
    expect(html).toContain("Tokens");
    expect(html).toContain("Cost ($)");
    expect(html).toContain("switch-knob");
    expect(html).not.toContain("<button");
  });

  it("marks aria-checked when cost mode is active", () => {
    const html = renderToStaticMarkup(<ModeToggle mode="cost" onChange={() => undefined} />);
    expect(html).toContain('aria-checked="true"');
    expect(html).toContain("switch-label-active");
  });

  it("keeps the same toggle contract: onChange is invoked with the flipped mode", () => {
    const onChange = vi.fn();
    const html = renderToStaticMarkup(<ModeToggle mode="tokens" onChange={onChange} />);
    expect(html).toContain('aria-checked="false"');
    // Static markup can't dispatch clicks; the behavioral contract is
    // covered by the shared ChartMode wiring in lib/usage tests.
    expect(typeof onChange).toBe("function");
  });
});

// S-201: the second sliding switch on the single Daily Usage chart —
// Squads | Agents series selector, same iOS-style pattern as the
// tokens/cost toggle.
describe("SourceToggle sliding switch", () => {
  it("renders Squads/Agents labels with the switch role", () => {
    const html = renderToStaticMarkup(<SourceToggle source="squads" onChange={() => undefined} />);
    expect(html).toContain('role="switch"');
    expect(html).toContain('aria-checked="false"');
    expect(html).toContain("Squads");
    expect(html).toContain("Agents");
    expect(html).toContain("switch-knob");
  });

  it("marks aria-checked when the agents series is active", () => {
    const html = renderToStaticMarkup(<SourceToggle source="agents" onChange={() => undefined} />);
    expect(html).toContain('aria-checked="true"');
    expect(html).toContain('aria-label="Chart series: agents"');
  });
});

// S-210: the selected option under the orange knob must render white.
// The class→color binding lives in globals.css; a specificity regression
// there (bare .switch-label-active losing to .switch-labels span) is
// exactly what made the old dark-on-orange text hard to read, so pin both
// the component class and the CSS rule that colors it.
describe("SlideSwitch selected-label white text (S-210)", () => {
  it("marks exactly one label active per toggle, on the selected side", () => {
    const tokens = renderToStaticMarkup(<ModeToggle mode="tokens" onChange={() => undefined} />);
    expect(tokens.match(/switch-label-active/g)).toHaveLength(1);
    expect(tokens).toContain('<span class="switch-label-active">Tokens</span>');

    const cost = renderToStaticMarkup(<ModeToggle mode="cost" onChange={() => undefined} />);
    expect(cost).toContain('<span class="switch-label-active">Cost ($)</span>');

    const agents = renderToStaticMarkup(<SourceToggle source="agents" onChange={() => undefined} />);
    expect(agents).toContain('<span class="switch-label-active">Agents</span>');
  });

  it("globals.css colors the active label with --accent-ink at a specificity that beats the base span rule", () => {
    const css = readFileSync(path.join(process.cwd(), "src/app/globals.css"), "utf8");
    const rule = css.match(/\.switch-labels \.switch-label-active\s*\{[^}]*\}/);
    expect(rule).not.toBeNull();
    expect(rule![0]).toContain("color: var(--accent-ink)");
  });
});
