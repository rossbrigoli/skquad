// S-189: real jsdom behaviour tests for ActivityFeed (previously 0%).
// Covers: empty state copy, action humanising, deep links per resource
// type, non-linkable resources, actor naming via nameFor/actor_type.
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { ActivityFeed } from "./ActivityFeed";
import type { AuditEntry } from "../lib/api";

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: "e1",
    actor_type: "agent",
    actor_id: "coder-1",
    action: "task.created",
    resource_type: "task",
    resource_id: "abcdef1234567890",
    timestamp: new Date().toISOString(),
    ...overrides,
  };
}

describe("ActivityFeed empty state", () => {
  it("renders the caller-supplied empty copy", () => {
    render(
      <ActivityFeed squadId="s1" entries={[]} emptyTitle="No activity" emptyHint="Go do things" />,
    );
    expect(screen.getByText("No activity")).toBeInTheDocument();
    expect(screen.getByText("Go do things")).toBeInTheDocument();
  });
});

describe("ActivityFeed entries", () => {
  it("humanises dotted actions ('task.created' → 'task created')", () => {
    render(<ActivityFeed squadId="s1" entries={[entry()]} emptyTitle="" emptyHint="" />);
    const title = document.querySelector(".entity-title");
    expect(title?.textContent).toContain("task created");
  });

  it("renders underscore actions with spaces in the verb", () => {
    render(
      <ActivityFeed
        squadId="s1"
        entries={[entry({ action: "log_in.session" })]}
        emptyTitle=""
        emptyHint=""
      />,
    );
    expect(screen.getByText("log in session", { exact: false })).toBeInTheDocument();
  });

  it("renders single-word actions verbatim", () => {
    render(
      <ActivityFeed squadId="s1" entries={[entry({ action: "ping" })]} emptyTitle="" emptyHint="" />
    );
    expect(screen.getByText("ping", { exact: false })).toBeInTheDocument();
  });

  it("deep-links task, agent and board resources into the squad routes", () => {
    render(
      <ActivityFeed
        squadId="sq9"
        entries={[
          entry({ id: "a", resource_type: "task", resource_id: "t1" }),
          entry({ id: "b", resource_type: "agent", resource_id: "ag1" }),
          entry({ id: "c", resource_type: "board" }),
        ]}
        emptyTitle=""
        emptyHint=""
      />,
    );
    const links = document.querySelectorAll("a.entity-row");
    expect(links).toHaveLength(3);
    expect(links[0]).toHaveAttribute("href", "/squads/sq9/tasks/t1");
    expect(links[1]).toHaveAttribute("href", "/squads/sq9/agents/ag1");
    expect(links[2]).toHaveAttribute("href", "/squads/sq9/board");
  });

  it("renders a plain row (no link) for unknown resource types", () => {
    render(
      <ActivityFeed
        squadId="s1"
        entries={[entry({ resource_type: "platform" })]}
        emptyTitle=""
        emptyHint=""
      />,
    );
    expect(document.querySelectorAll("a.entity-row")).toHaveLength(0);
    expect(document.querySelectorAll("div.entity-row")).toHaveLength(1);
  });

  it("shows the actor id for user actors and the actor type otherwise", () => {
    render(
      <ActivityFeed
        squadId="s1"
        entries={[
          entry({ id: "u", actor_type: "user", actor_id: "ross" }),
          entry({ id: "g", actor_type: "agent", actor_id: "coder-1" }),
        ]}
        emptyTitle=""
        emptyHint=""
      />,
    );
    expect(screen.getByText("ross", { exact: true })).toBeInTheDocument();
    expect(screen.getByText("agent", { exact: true })).toBeInTheDocument();
  });

  it("prefers nameFor() over the raw actor fields", () => {
    render(
      <ActivityFeed
        squadId="s1"
        entries={[entry({ actor_type: "user", actor_id: "u-123" })]}
        emptyTitle=""
        emptyHint=""
        nameFor={(e) => (e.actor_id === "u-123" ? "Ross B" : undefined)}
      />,
    );
    expect(screen.getByText("Ross B", { exact: true })).toBeInTheDocument();
    expect(screen.queryByText("u-123")).not.toBeInTheDocument();
  });

  it("truncates the resource id to 8 chars in the meta line", () => {
    render(
      <ActivityFeed
        squadId="s1"
        entries={[entry({ resource_id: "abcdef1234567890" })]}
        emptyTitle=""
        emptyHint=""
      />,
    );
    expect(screen.getByText(/abcdef12/)).toHaveTextContent("abcdef12");
    expect(screen.getByText(/abcdef12/).textContent).not.toContain("34567890");
  });
});
