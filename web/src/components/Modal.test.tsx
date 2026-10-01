// S-186: the effective-prompt preview needed a wider dialog. Modal gains a
// `wide` prop that adds .modal-wide to .modal-card (CSS: max-width
// min(70vw, 1200px), near-full-width on mobile). Rendered to static markup
// per the repo's node-environment test philosophy.
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { Modal } from "./Modal";

function render(props: { wide?: boolean; danger?: boolean } = {}): string {
  return renderToStaticMarkup(
    createElement(Modal, {
      title: "Effective prompt — test agent",
      onClose: () => undefined,
      children: "body",
      ...props,
    }),
  );
}

describe("Modal width variants", () => {
  it("defaults to the standard-width card (no .modal-wide)", () => {
    const html = render();
    expect(html).toContain('class="modal-card"');
    expect(html).not.toContain("modal-wide");
  });

  it("adds .modal-wide when wide is set", () => {
    const html = render({ wide: true });
    expect(html).toContain("modal-card modal-wide");
  });

  it("combines .modal-wide with .modal-danger", () => {
    const html = render({ wide: true, danger: true });
    expect(html).toContain("modal-card modal-danger modal-wide");
  });
});
