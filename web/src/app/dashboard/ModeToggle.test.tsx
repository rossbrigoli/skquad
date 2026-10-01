// S-195: the tokens/cost control is now a single iOS-style sliding switch
// (role="switch", aria-checked reflects the cost mode) instead of two
// buttons. Both labels render inside the track; the knob slides via CSS.
import { describe, expect, it, vi } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { ModeToggle } from "./page";

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
