// S-204: tile grid rendering — built-in vs registered presentation.
// Rendered with renderToStaticMarkup (node env, no DOM): the grid is a
// pure function of its items, so this locks the tile contract without
// jsdom.
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { ToolTilesGrid } from "./ToolTiles";
import type { ToolItem } from "../lib/toolsPage";

const items: ToolItem[] = [
  {
    id: "exec",
    kind: "builtin",
    name: "exec",
    description: "Run terminal commands inside the agent pod sandbox.",
    enabled: true,
  },
  {
    id: "rt-1",
    kind: "registry",
    name: "Jira",
    description: "Atlassian Jira integration",
    enabled: true,
  },
  {
    id: "rt-2",
    kind: "registry",
    name: "Legacy",
    description: "",
    enabled: false,
  },
];

const html = renderToStaticMarkup(createElement(ToolTilesGrid, { items }));

describe("ToolTilesGrid (S-204)", () => {
  it("renders one tile per tool inside the grid", () => {
    expect(html).toContain('class="tool-grid"');
    expect((html.match(/class="tool-tile"/g) ?? []).length).toBe(3);
  });

  it("every tile links to the tool configuration route", () => {
    expect(html).toContain('href="/settings/resources/tools/exec"');
    expect(html).toContain('href="/settings/resources/tools/rt-1"');
    expect(html).toContain('href="/settings/resources/tools/rt-2"');
  });

  it("built-in tiles wear the tool (wrench) icon, not the Skquad logo (S-241)", () => {
    const execTile = html.split('class="tool-tile"')[1];
    expect(execTile).not.toContain("skquad-logo");
    expect(execTile).toContain("<svg");
    expect(execTile).toContain(">Built-in<");
  });

  it("every tile carries an enable/disable switch beside the link (S-241)", () => {
    // One switch per tile, as a sibling of the <a> (never nested inside it).
    expect((html.match(/role="switch"/g) ?? []).length).toBe(3);
    expect(html).toContain('class="tool-tile-toggle"');
    // The toggle span follows the closed <a> — it is a sibling, not nested.
    expect(html).toContain("</a><span class=\"tool-tile-toggle\">");
  });

  it("switch aria-checked mirrors the tool's enabled state (S-241)", () => {
    const toggleHtml = renderToStaticMarkup(
      createElement(ToolTilesGrid, { items, onToggle: () => {} }),
    );
    const execSwitch = toggleHtml.split('role="switch"')[1];
    expect(execSwitch.startsWith(' aria-checked="true"')).toBe(true);
    expect(execSwitch).toContain('aria-label="Disable exec"');
    const legacySwitch = toggleHtml.split('role="switch"')[3];
    expect(legacySwitch.startsWith(' aria-checked="false"')).toBe(true);
    // Legacy is a registry tool — it gets the registry label, not "Enable …".
    expect(legacySwitch).toContain('aria-label="Legacy: enable/disable is managed on the registry');
  });

  it("registry tiles render the switch disabled — no per-tile enable API (S-241)", () => {
    const toggleHtml = renderToStaticMarkup(
      createElement(ToolTilesGrid, { items, onToggle: () => {} }),
    );
    const jiraSwitch = toggleHtml.split('role="switch"')[2];
    expect(jiraSwitch).toContain('disabled=""');
    expect(jiraSwitch).toContain("registry");
    const execSwitch = toggleHtml.split('role="switch"')[1];
    expect(execSwitch).not.toContain('disabled=""');
  });

  it("registered tiles use the placeholder glyph, not the Skquad logo", () => {
    const jiraTile = html.split('class="tool-tile"')[2];
    expect(jiraTile).not.toContain("skquad-logo");
    expect(jiraTile).toContain("<svg");
  });

  it("registered tiles carry no Built-in badge", () => {
    const jiraTile = html.split('class="tool-tile"')[2];
    expect(jiraTile).not.toContain("Built-in");
  });

  it("shows the short description on each tile", () => {
    expect(html).toContain("Atlassian Jira integration");
    expect(html).toContain("Run terminal commands");
  });

  it("disabled tools surface a disabled marker; enabled ones don't", () => {
    expect((html.match(/tool-tile-state/g) ?? []).length).toBe(1);
    expect(html).toContain(">disabled<");
  });

  it("empty description renders an em-dash placeholder", () => {
    const legacyTile = html.split('class="tool-tile"')[3];
    expect(legacyTile).toContain("—");
  });
});
