// S-189: jsdom behaviour tests for AppShell (previously 0%). Covers:
// primary rail + active route, inbox badge, squads/agents expandable
// groups (auto-expand on route + manual toggle), breadcrumbs, the mobile
// drawer toggle, and SquadTabs visibility per route.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  pathname: "/dashboard",
  inboxUnread: 0,
  user: null as null | { id: string; name?: string; role?: string },
}));

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));

vi.mock("next/image", () => ({
  default: ({ src, alt }: { src: string; alt: string }) => <img src={src} alt={alt ?? ""} />,
}));

vi.mock("next/navigation", () => ({
  usePathname: () => env.pathname,
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ user: env.user, logout: vi.fn(), token: "t", mode: "token" }),
}));

vi.mock("../lib/useAttention", () => ({
  useAttention: () => ({ inboxUnread: env.inboxUnread }),
}));

const DATA: Record<string, unknown> = {
  "/squads": [{ id: "sq1", name: "Alpha Squad" }],
  "/squads?all=true": [
    { id: "sq1", name: "Alpha Squad", owner_id: "u2" },
    { id: "sq2", name: "Beta Squad", owner_id: "u3" },
  ],
  "/users": [
    { id: "u2", name: "Ada", email: "ada@example.com" },
    { id: "u3", email: "bob@example.com" },
  ],
  "/tasks/t9": { id: "t9", task_number: 42 },
  "/dashboard": {
    squads: [
      {
        id: "sq1",
        name: "Alpha Squad",
        agents: [
          { id: "ag1", name: "coder-1" },
          { id: "ag2", name: "review-2" },
        ],
      },
    ],
  },
  "/squads/sq1/agents": [{ id: "ag1", name: "coder-1" }],
};

vi.mock("../lib/useApi", () => ({
  useApi: (path: string) => ({
    data: path ? (DATA[path] ?? null) : null,
    loading: false,
    error: "",
    refresh: vi.fn(),
  }),
}));

vi.mock("./NotificationBell", () => ({
  NotificationBell: () => <div data-testid="bell" />,
}));
vi.mock("./ThemeToggle", () => ({
  ThemeToggle: () => <div data-testid="theme-toggle" />,
}));
vi.mock("./SquadTabs", () => ({
  SquadTabs: () => <div data-testid="squad-tabs" />,
}));
vi.mock("./UserMenu", () => ({
  UserMenu: () => <div data-testid="user-menu" />,
}));

import { AppShell } from "./AppShell";

beforeEach(() => {
  env.pathname = "/dashboard";
  env.inboxUnread = 0;
  env.user = { id: "u1", name: "Ross" };
});

describe("AppShell primary rail", () => {
  it("renders brand + nav and marks the active route", () => {
    render(
      <AppShell>
        <p>page body</p>
      </AppShell>,
    );
    const dash = screen.getByRole("link", { name: /Dashboard/ });
    expect(dash).toHaveAttribute("href", "/dashboard");
    expect(dash.className).toContain("active");
    expect(screen.getByRole("link", { name: /Inbox/ }).className).not.toContain("active");
    expect(screen.getByText("page body")).toBeInTheDocument();
  });

  it("shows the unread inbox badge only when > 0", () => {
    const { unmount } = render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    expect(document.querySelector(".nav-badge")).toBeNull();
    unmount();
    env.inboxUnread = 4;
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    expect(document.querySelector(".nav-badge")).toHaveTextContent("4");
  });

  it("auto-expands the Squads group inside a squad route and lists sub-items", () => {
    env.pathname = "/squads/sq1/board";
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    const chevron = screen.getByRole("button", { name: "Collapse squads" });
    expect(chevron).toHaveAttribute("aria-expanded", "true");
    const rail = within(screen.getByRole("navigation", { name: "Primary navigation" }));
    expect(rail.getByRole("link", { name: "Alpha Squad" })).toHaveAttribute(
      "href",
      "/squads/sq1",
    );
  });

  it("collapses the Squads group when the chevron is clicked", async () => {
    env.pathname = "/squads/sq1/board";
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Collapse squads" }));
    expect(screen.getByRole("button", { name: "Expand squads" })).toHaveAttribute(
      "aria-expanded", "false",
    );
    const rail = within(screen.getByRole("navigation", { name: "Primary navigation" }));
    expect(rail.queryByRole("link", { name: "Alpha Squad" })).not.toBeInTheDocument();
  });

  it("expands the Agents group and lists agents grouped by squad", async () => {
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Expand agents" }));
    expect(document.querySelectorAll(".nav-subgroup")).toHaveLength(1);
    expect(document.querySelector(".nav-subgroup-label")).toHaveTextContent("Alpha Squad");
    expect(screen.getByRole("link", { name: "coder-1" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "review-2" })).toBeInTheDocument();
  });

  it("toggles the mobile drawer open and closed", async () => {
    const { container } = render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    const btn = screen.getByRole("button", { name: "Open menu" });
    expect(btn).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(btn);
    expect(container.querySelector(".shell")).toHaveClass("drawer-open");
    await userEvent.click(screen.getByRole("button", { name: "Close menu" }));
    expect(container.querySelector(".shell")).not.toHaveClass("drawer-open");
  });

  it("renders breadcrumbs with the current page marked", () => {
    env.pathname = "/settings";
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    const crumbNav = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(crumbNav).toHaveTextContent("Settings");
    expect(crumbNav.querySelector('[aria-current="page"]')).toHaveTextContent("Settings");
  });

  it("shows SquadTabs on squad routes but not on agent detail routes", () => {
    env.pathname = "/squads/sq1/board";
    const { unmount } = render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    expect(screen.getByTestId("squad-tabs")).toBeInTheDocument();
    unmount();
    env.pathname = "/squads/sq1/agents/ag1";
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    expect(screen.queryByTestId("squad-tabs")).not.toBeInTheDocument();
  });

  it("as a platform admin lists all squads with owner labels", () => {
    env.user = { id: "u1", name: "Ross", role: "platform_admin" };
    env.pathname = "/squads/sq1/board";
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    const rail = within(screen.getByRole("navigation", { name: "Primary navigation" }));
    const link = rail.getByRole("link", { name: /Alpha Squad/ });
    expect(link).toHaveAttribute("href", "/squads/sq1");
    // Owner label resolved from /users (name preferred over email prefix).
    expect(link.textContent).toContain("Ada");
    // Owner without a name falls back to the email prefix.
    expect(rail.getByRole("link", { name: /Beta Squad/ }).textContent).toContain("bob");
  });

  it("closes the open drawer when the route changes", async () => {
    const { container, rerender } = render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Open menu" }));
    expect(container.querySelector(".shell")).toHaveClass("drawer-open");
    env.pathname = "/settings";
    rerender(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    expect(container.querySelector(".shell")).not.toHaveClass("drawer-open");
  });

  it("shows the T-<n> task ref in breadcrumbs on task routes", () => {
    env.pathname = "/squads/sq1/tasks/t9";
    render(
      <AppShell>
        <p>x</p>
      </AppShell>,
    );
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent("T-42");
  });
});
