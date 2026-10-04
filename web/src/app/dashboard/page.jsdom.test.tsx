// S-189: jsdom behaviour tests for the Dashboard page. Covers the
// loading empty state, admin vs member tiles, the two sliding chart
// toggles (series + metric), squad/provider/resource sections with
// their empty states, and error notices for both API calls.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  dashboard: null as Record<string, unknown> | null,
  usage: null as Record<string, unknown> | null,
  error: "",
  usageError: "",
  refresh: vi.fn(),
}));

vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../components/BarChart", () => ({
  BarChart: ({ model, formatValue }: { model: unknown; formatValue: (n: number) => string }) => (
    <div
      data-testid="barchart"
      data-formatted={formatValue(1234)}
      data-series={(model as { series?: unknown[] })?.series?.length ?? 0}
    />
  ),
}));

vi.mock("../../lib/useApi", () => ({
  useApi: (path: string) => {
    if (path === "/dashboard") {
      return { data: env.dashboard, loading: !env.dashboard && !env.error, error: env.error, refresh: env.refresh };
    }
    return { data: env.usage, loading: false, error: env.usageError, refresh: env.refresh };
  },
}));

import DashboardPage from "./page";

const memberDashboard = {
  scope: "member",
  squads: [
    {
      id: "sq1",
      name: "Alpha",
      cost: { cost: 4.25, currency: "USD", tokens: 1000 },
      owner_name: "Ross",
      task_counts: { todo: 2, "in-progress": 1, done: 5 },
      agents: [
        { id: "ag1", squad_id: "sq1", name: "coder", status: "running", cost: { cost: 1, currency: "USD", tokens: 10 } },
        { id: "ag2", squad_id: "sq1", name: "broken", status: "error" },
      ],
    },
  ],
  providers: [
    { id: "p1", name: "OpenAI", kind: "openai", latency_ms: 120, status: "active", online: true },
    { id: "p2", name: "Local", kind: "ollama", status: "active", online: false, error: "timeout" },
  ],
  resources: [{ id: "r1", name: "deploy-skill", type: "skill", status: "active" }],
};

const adminDashboard = { ...memberDashboard, scope: "all" };

const usagePayload = {
  currency: "USD",
  squad_mtd_cost: 2.5,
  days: ["2026-10-01", "2026-10-02"],
  by_squad: [{ id: "sq1", name: "Alpha", daily: [100, 200], daily_cost: [0.5, 1.5] }],
  by_agent: [{ id: "ag1", name: "coder", daily: [100, 200], daily_cost: [0.5, 1.5] }],
  providers: [
    {
      provider_id: "p1",
      provider_name: "OpenAI",
      cost: 3.5,
      tokens: 9000,
      models: [{ model: "gpt-x", cost: 3.5, tokens: 9000 }],
    },
  ],
  platform: { total_cost: 100, mtd_cost: 20, users: 7, agents: 12 },
};

beforeEach(() => {
  env.dashboard = memberDashboard;
  env.usage = usagePayload;
  env.error = "";
  env.usageError = "";
  env.refresh = vi.fn();
});

describe("DashboardPage top-level states", () => {
  it("shows the loading empty state before first data", () => {
    env.dashboard = null;
    render(<DashboardPage />);
    expect(screen.getByText("Loading your dashboard…")).toBeInTheDocument();
  });

  it("surfaces both the main and usage errors", () => {
    env.error = "api down";
    env.usageError = "usage down";
    render(<DashboardPage />);
    expect(screen.getByText("api down")).toBeInTheDocument();
    expect(screen.getByText(/Usage series unavailable: usage down/)).toBeInTheDocument();
  });

  it("refresh button wires to the hook refresh", async () => {
    const user = userEvent.setup();
    render(<DashboardPage />);
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    expect(env.refresh).toHaveBeenCalledTimes(1);
  });
});

describe("DashboardPage metric tiles", () => {
  it("member sees owned tiles without platform extras", () => {
    render(<DashboardPage />);
    expect(screen.getByText("owned + granted")).toBeInTheDocument();
    expect(screen.queryByText("Platform total cost")).not.toBeInTheDocument();
    // 2 agents total, 1 running, 1 error.
    expect(screen.getByText("2 agents total")).toBeInTheDocument();
    expect(screen.getByText("check the squads below")).toBeInTheDocument();
  });

  it("admin sees platform-wide tiles", () => {
    env.dashboard = adminDashboard;
    render(<DashboardPage />);
    expect(screen.getByText("all squads (platform admin)")).toBeInTheDocument();
    expect(screen.getByText("Platform MTD cost")).toBeInTheDocument();
    expect(screen.getByText("Users")).toBeInTheDocument();
  });
});

describe("DashboardPage chart toggles", () => {
  it("source toggle switches the chart series between squads and agents", async () => {
    const user = userEvent.setup();
    render(<DashboardPage />);
    const sourceSwitch = screen.getByRole("switch", { name: "Chart series: squads" });
    expect(sourceSwitch).toHaveAttribute("aria-checked", "false");
    await user.click(sourceSwitch);
    expect(screen.getByRole("switch", { name: "Chart series: agents" })).toHaveAttribute("aria-checked", "true");
  });

  it("mode toggle flips tokens → cost via keyboard", async () => {
    const user = userEvent.setup();
    render(<DashboardPage />);
    const modeSwitch = screen.getByRole("switch", { name: "Chart metric: tokens" });
    await user.click(modeSwitch);
    expect(screen.getByRole("switch", { name: "Chart metric: cost" })).toHaveAttribute("aria-checked", "true");
    // The cost formatter is now the active value formatter.
    expect(screen.getByTestId("barchart").getAttribute("data-formatted")).toContain("USD 1234");
    // Keyboard activation (Enter) flips it back.
    await user.keyboard("{Enter}");
    expect(screen.getByRole("switch", { name: "Chart metric: tokens" })).toHaveAttribute("aria-checked", "false");
  });
});

describe("DashboardPage sections", () => {
  it("renders squad rows with counts, owner, agents and costs", () => {
    render(<DashboardPage />);
    const squadLink = screen.getByRole("link", { name: "Alpha" });
    expect(squadLink).toHaveAttribute("href", "/squads/sq1");
    expect(squadLink.closest(".entity-row")).toHaveTextContent("2 todo · 1 in progress");
    expect(squadLink.closest(".entity-row")).toHaveTextContent("owner: Ross");
    expect(screen.getByRole("link", { name: "coder" })).toHaveAttribute("href", "/squads/sq1/agents/ag1");
  });

  it("renders provider rows with liveness chip and MTD usage", () => {
    render(<DashboardPage />);
    const row = screen.getByText("OpenAI").closest(".entity-row") as HTMLElement;
    expect(row).toHaveTextContent("MTD");
    expect(row).toHaveTextContent("9K tokens");
    const errRow = screen.getByText("Local").closest(".entity-row") as HTMLElement;
    expect(errRow).toHaveTextContent("timeout");
    expect(errRow).toHaveTextContent("no usage this month");
  });

  it("renders resource rows with type spacing", () => {
    render(<DashboardPage />);
    const row = screen.getByText("deploy-skill").closest(".entity-row") as HTMLElement;
    expect(row).toHaveTextContent("deploy-skill");
    expect(row.textContent).toContain("skill");
  });

  it("empty sections fall back to EmptyState", () => {
    env.dashboard = { scope: "member", squads: [], providers: [], resources: [] };
    render(<DashboardPage />);
    expect(screen.getByText("No squads yet")).toBeInTheDocument();
    expect(screen.getByText("No providers registered")).toBeInTheDocument();
    expect(screen.getByText("No resources registered")).toBeInTheDocument();
  });

  it("usage feed missing falls back to USD and zero MTD", () => {
    env.usage = null;
    render(<DashboardPage />);
    expect(screen.getByText("MTD cost").closest(".metric")).toHaveTextContent("USD");
  });

  it("space key activates the sliding switch", async () => {
    const user = userEvent.setup();
    render(<DashboardPage />);
    const modeSwitch = screen.getByRole("switch", { name: "Chart metric: tokens" });
    modeSwitch.focus();
    await user.keyboard(" ");
    expect(screen.getByRole("switch", { name: "Chart metric: cost" })).toHaveAttribute("aria-checked", "true");
  });

  it("squad rows without owner/agents/cost degrade gracefully", () => {
    env.dashboard = {
      scope: "member",
      squads: [{ id: "sq9", name: "Sparse" }],
      providers: [{ id: "p9", name: "Bare", kind: "other" }],
      resources: [],
    };
    render(<DashboardPage />);
    const row = screen.getByRole("link", { name: "Sparse" }).closest(".entity-row") as HTMLElement;
    expect(row).toHaveTextContent("no agents in this squad");
    expect(row).toHaveTextContent("0 todo");
    const prow = screen.getByText("Bare").closest(".entity-row") as HTMLElement;
    expect(prow).toHaveTextContent("no usage this month");
    // No error agents → healthy sub-label.
    expect(screen.getByText("all healthy")).toBeInTheDocument();
  });
});
