// S-179: node-environment render tests for the inline squad mission
// config (same philosophy as MarkdownMessage.test.tsx — react-dom/server,
// no jsdom).
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { SquadMissionConfig } from "./SquadMissionConfig";
import type { Squad } from "../lib/api";

function render(mission: string | undefined): string {
  const squad = { id: "sq1", name: "Test Squad", mission } as Squad;
  return renderToStaticMarkup(
    createElement(SquadMissionConfig, {
      squad,
      token: "t",
      onSaved: () => undefined,
    }),
  );
}

describe("SquadMissionConfig", () => {
  it("renders the mission textarea with the current mission", () => {
    const html = render("Ship it safely.");
    expect(html).toContain("Mission");
    expect(html).toContain("Ship it safely.");
    expect(html).toContain("<textarea");
  });

  it("renders a Save button next to the textarea", () => {
    const html = render("Some mission");
    expect(html).toContain("Save");
    // The button sits in the same flex row as the textarea.
    const rowIdx = html.indexOf("display:flex");
    const textareaIdx = html.indexOf("<textarea");
    const buttonIdx = html.indexOf("<button");
    expect(rowIdx).toBeGreaterThan(-1);
    expect(textareaIdx).toBeGreaterThan(rowIdx);
    expect(buttonIdx).toBeGreaterThan(textareaIdx);
  });

  it("does not render a squad name input (name is immutable)", () => {
    const html = render("m");
    expect(html).not.toContain("<input");
  });

  it("Save is disabled when nothing changed", () => {
    const html = render("Unchanged mission");
    expect(html).toMatch(/<button[^>]*disabled/);
  });

  it("renders empty textarea when mission is undefined", () => {
    const html = render(undefined);
    expect(html).toContain("<textarea");
    expect(html).toContain("What is this squad for?");
  });
});
