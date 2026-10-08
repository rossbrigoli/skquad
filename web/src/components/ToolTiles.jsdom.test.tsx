// S-241: jsdom interaction tests for the tile toggle. The node-env
// ToolTiles.test.tsx locks the static contract; this covers behaviour:
// the toggle fires onToggle without bubbling into the tile link
// (no navigation), pending disables it, and registry switches stay
// inert.
import React from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ToolTilesGrid } from "./ToolTiles";
import type { ToolItem } from "../lib/toolsPage";

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));

const items: ToolItem[] = [
  { id: "exec", kind: "builtin", name: "exec", description: "Run commands", enabled: true },
  { id: "rt-1", kind: "registry", name: "Jira", description: "Atlassian Jira", enabled: true },
];

describe("ToolTiles toggle (S-241, jsdom)", () => {
  it("toggle click fires onToggle with the tool and the flipped state", async () => {
    const onToggle = vi.fn();
    render(<ToolTilesGrid items={items} onToggle={onToggle} />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("switch", { name: "Disable exec" }));
    expect(onToggle).toHaveBeenCalledTimes(1);
    expect(onToggle).toHaveBeenCalledWith(items[0], false);
  });

  it("toggle click does not propagate to ancestor click handlers (no navigation)", async () => {
    const onToggle = vi.fn();
    const ancestorClick = vi.fn();
    render(
      <div onClick={ancestorClick}>
        <ToolTilesGrid items={items} onToggle={onToggle} />
      </div>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("switch", { name: "Disable exec" }));
    expect(onToggle).toHaveBeenCalledTimes(1);
    // stopPropagation in ToggleSwitch keeps the click from reaching the
    // tile link / any ancestor handler.
    expect(ancestorClick).not.toHaveBeenCalled();
  });

  it("clicking the tile link itself still reaches ancestors (link untouched)", async () => {
    const ancestorClick = vi.fn();
    render(
      <div onClick={ancestorClick}>
        <ToolTilesGrid items={items} />
      </div>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("link", { name: "Configure exec" }));
    expect(ancestorClick).toHaveBeenCalledTimes(1);
  });

  it("pending disables only the pending tile's switch", () => {
    render(<ToolTilesGrid items={items} onToggle={vi.fn()} pendingToolId="exec" />);
    expect(screen.getByRole("switch", { name: "Disable exec" })).toBeDisabled();
  });

  it("registry switches stay disabled even without pending and never fire onToggle", async () => {
    const onToggle = vi.fn();
    render(<ToolTilesGrid items={items} onToggle={onToggle} />);
    const jiraSwitch = screen.getByRole("switch", { name: /Jira/ });
    expect(jiraSwitch).toBeDisabled();
    const user = userEvent.setup();
    await user.click(jiraSwitch);
    expect(onToggle).not.toHaveBeenCalled();
  });

  it("disabled tool shows the disabled marker and an Enable label", () => {
    const disabledItems: ToolItem[] = [
      { ...items[0], enabled: false },
    ];
    render(<ToolTilesGrid items={disabledItems} onToggle={vi.fn()} />);
    expect(screen.getByText("disabled")).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Enable exec" })).toBeEnabled();
  });
});
