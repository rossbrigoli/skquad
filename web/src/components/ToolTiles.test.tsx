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

  it("built-in tiles wear the Skquad logo and a Built-in badge", () => {
    const execTile = html.split('class="tool-tile"')[1];
    expect(execTile).toContain("skquad-logo-64.png");
    expect(execTile).toContain(">Built-in<");
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
