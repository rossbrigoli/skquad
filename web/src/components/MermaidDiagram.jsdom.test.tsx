// S-246: jsdom behaviour tests for MermaidDiagram.
// The component lazy-loads mermaid via dynamic import, so we mock the module
// with vi.mock (factory shape matches `await import("mermaid")`). Module-level
// caches are reset between tests via vi.resetModules() + dynamic import.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { render, waitFor } from "@testing-library/react";

const mockInitialize = vi.fn();
const mockRender = vi.fn();

vi.mock("mermaid", () => ({
  default: {
    initialize: (...args: unknown[]) => mockInitialize(...args),
    render: (...args: unknown[]) => mockRender(...args),
  },
}));

async function freshComponent() {
  vi.resetModules();
  mockInitialize.mockClear();
  mockRender.mockClear();
  return (await import("./MermaidDiagram")).MermaidDiagram;
}

describe("MermaidDiagram", () => {
  beforeEach(() => {
    document.body.innerHTML = "";
  });

  it("initialises mermaid once with strict security and no auto-load", async () => {
    mockRender.mockResolvedValue({ svg: "<svg><text>ok</text></svg>" });
    const MermaidDiagram = await freshComponent();
    render(createElement(MermaidDiagram, { code: "graph TD; A-->B;" }));
    await waitFor(() => expect(mockInitialize).toHaveBeenCalled());
    expect(mockInitialize).toHaveBeenCalledWith(
      expect.objectContaining({
        startOnLoad: false,
        securityLevel: "strict",
        theme: "neutral",
      }),
    );
  });

  it("renders the sanitized SVG into the .mermaid-diagram container", async () => {
    mockRender.mockResolvedValue({
      svg: '<svg data-diagram-type="flowchart"><g><text>Hello</text></g></svg>',
    });
    const MermaidDiagram = await freshComponent();
    const { container } = render(
      createElement(MermaidDiagram, { code: "graph TD; A-->B;" }),
    );
    await waitFor(() =>
      expect(container.querySelector(".mermaid-diagram svg")).toBeTruthy(),
    );
    expect(container.textContent).toContain("Hello");
  });

  it("strips script tags that sneak past mermaid (DOMPurify layer)", async () => {
    mockRender.mockResolvedValue({
      svg: '<svg><text>safe</text><script>alert("xss")</script></svg>',
    });
    const MermaidDiagram = await freshComponent();
    const { container } = render(
      createElement(MermaidDiagram, { code: "graph TD; A-->B;" }),
    );
    await waitFor(() => expect(container.querySelector("svg")).toBeTruthy());
    expect(container.querySelector("script")).toBeNull();
    expect(container.innerHTML).not.toContain("alert");
  });

  it("falls back to a copyable code block when render fails", async () => {
    mockRender.mockRejectedValue(new Error("Parse error on line 1"));
    const MermaidDiagram = await freshComponent();
    const { container } = render(
      createElement(MermaidDiagram, { code: "graph TD; this is broken ---" }),
    );
    await waitFor(() =>
      expect(container.querySelector("pre.mermaid-fallback")).toBeTruthy(),
    );
    expect(container.querySelector("code.language-mermaid")?.textContent).toBe(
      "graph TD; this is broken ---",
    );
  });

  it("serves identical code from cache without re-rendering", async () => {
    mockRender.mockResolvedValue({ svg: "<svg><text>cached</text></svg>" });
    const MermaidDiagram = await freshComponent();
    const first = render(createElement(MermaidDiagram, { code: "graph TD; X-->Y;" }));
    await waitFor(() => expect(first.container.querySelector("svg")).toBeTruthy());
    expect(mockRender).toHaveBeenCalledTimes(1);

    const second = render(createElement(MermaidDiagram, { code: "graph TD; X-->Y;" }));
    // Synchronous cache hit in useState initialiser — no extra render call.
    expect(second.container.querySelector(".mermaid-diagram svg")).toBeTruthy();
    expect(mockRender).toHaveBeenCalledTimes(1);
  });

  it("ignores results after unmount (no state update on dead component)", async () => {
    let resolveRender: (v: { svg: string }) => void = () => {};
    mockRender.mockReturnValue(
      new Promise((res) => {
        resolveRender = res;
      }),
    );
    const MermaidDiagram = await freshComponent();
    const { unmount } = render(
      createElement(MermaidDiagram, { code: "graph TD; A-->B;" }),
    );
    unmount();
    // Resolving after unmount must not throw or warn.
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    resolveRender({ svg: "<svg><text>late</text></svg>" });
    await new Promise((r) => setTimeout(r, 20));
    expect(errSpy).not.toHaveBeenCalled();
    errSpy.mockRestore();
  });
});
