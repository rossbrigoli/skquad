// TG-9 slice C: jsdom behaviour tests for the Audit & Metering page.
// Locks: admin vs owner scoping (endpoint selection), the filter bar →
// client-side filtering, human-readable decision chips, the detail
// drawer, the per-resource metering tab, and loading/empty/error states.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  role: "platform_admin" as string,
  squads: [] as unknown[],
  auditEntries: [] as unknown[],
  auditError: "" as string,
  auditLoading: false as boolean,
  meteringRows: [] as unknown[],
  paths: [] as string[],
}));

vi.mock("../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../lib/auth", () => ({
  useAuth: () => ({
    token: "tok",
    mode: "token",
    user: { id: "me", email: "me@x", name: "Me", role: env.role },
    authed: true,
  }),
}));

vi.mock("../../lib/useApi", () => ({
  useApi: (path: string) => {
    env.paths.push(path);
    if (path.startsWith("/squads?all=true") || path === "/squads") {
      return { data: env.squads, loading: false, error: "", refresh: vi.fn() };
    }
    if (path === "") {
      return { data: null, loading: false, error: "", refresh: vi.fn() };
    }
    if (path.startsWith("/metering/resources")) {
      return { data: { rows: env.meteringRows }, loading: false, error: "", refresh: vi.fn() };
    }
    return {
      data: env.auditError ? null : env.auditEntries,
      loading: env.auditLoading,
      error: env.auditError,
      refresh: vi.fn(),
    };
  },
}));

import AuditPage from "./page";

const DENIED_ENTRY = {
  id: "1",
  actor_type: "agent",
  actor_id: "ag1",
  actor_display: "Bob",
  action: "confirmation.deny",
  resource_type: "tool_resource",
  resource_id: "res-aaaa-1111",
  timestamp: "2026-10-05T00:00:00Z",
  metadata: { reason: "denied_replayed", risk_tier: "high" },
};

const ALLOWED_ENTRY = {
  id: "2",
  actor_type: "user",
  actor_id: "u1",
  actor_display: "Ross",
  action: "task.create",
  resource_type: "task",
  resource_id: "task-2",
  timestamp: "2026-10-06T00:00:00Z",
};

beforeEach(() => {
  env.role = "platform_admin";
  env.squads = [{ id: "s1", name: "Alpha Squad" }];
  env.auditEntries = [DENIED_ENTRY, ALLOWED_ENTRY];
  env.auditError = "";
  env.auditLoading = false;
  env.meteringRows = [];
  env.paths = [];
});

describe("admin scoping", () => {
  it("fetches the global audit endpoint and renders both rows", async () => {
    render(<AuditPage />);
    expect(await screen.findByText("Bob")).toBeInTheDocument();
    expect(screen.getByText("Ross")).toBeInTheDocument();
    expect(env.paths).toContain("/audit?limit=200");
  });

  it("renders the human-readable denial chip and tier", async () => {
    render(<AuditPage />);
    const chip = await screen.findByText("already used");
    expect(chip.className).toContain("chip-deny");
    expect(screen.getByText("high")).toBeInTheDocument();
  });

  it("squad filter narrows the server query to squad_id", async () => {
    render(<AuditPage />);
    const select = await screen.findByLabelText("Squad filter");
    await userEvent.selectOptions(select, "s1");
    expect(env.paths).toContain("/audit?squad_id=s1&limit=200");
  });
});

describe("detail drawer", () => {
  it("opens on row click with full fields and closes", async () => {
    render(<AuditPage />);
    const row = await screen.findByText("Bob");
    await userEvent.click(row);
    const drawer = await screen.findByLabelText("Event detail");
    expect(within(drawer).getByText("confirmation.deny")).toBeInTheDocument();
    expect(within(drawer).getByText("res-aaaa-1111")).toBeInTheDocument();
    expect(within(drawer).getByText("denied_replayed")).toBeInTheDocument();
    await userEvent.click(within(drawer).getByLabelText("Close event detail"));
    expect(screen.queryByLabelText("Event detail")).toBeNull();
  });
});

describe("client-side filters", () => {
  it("agent filter keeps only that agent's rows", async () => {
    render(<AuditPage />);
    await screen.findByText("Bob");
    await userEvent.type(screen.getByLabelText("Agent filter"), "ag1");
    expect(screen.getByText("Bob")).toBeInTheDocument();
    expect(screen.queryByText("Ross")).toBeNull();
  });

  it("decision filter keeps only denied events", async () => {
    render(<AuditPage />);
    await screen.findByText("Bob");
    await userEvent.selectOptions(screen.getByLabelText("Decision filter"), "deny");
    expect(screen.getByText("Bob")).toBeInTheDocument();
    expect(screen.queryByText("Ross")).toBeNull();
  });

  it("time range excludes older events", async () => {
    render(<AuditPage />);
    await screen.findByText("Bob");
    await userEvent.type(screen.getByLabelText("From time"), "2026-10-05T23:59");
    expect(screen.queryByText("Bob")).toBeNull();
    expect(screen.getByText("Ross")).toBeInTheDocument();
  });

  it("To time excludes newer events", async () => {
    render(<AuditPage />);
    await screen.findByText("Bob");
    await userEvent.type(screen.getByLabelText("To time"), "2026-10-05T12:00");
    expect(screen.getByText("Bob")).toBeInTheDocument();
    expect(screen.queryByText("Ross")).toBeNull();
  });

  it("window select changes the server limit", async () => {
    render(<AuditPage />);
    await screen.findByText("Bob");
    await userEvent.selectOptions(screen.getByLabelText("Event window"), "500");
    expect(env.paths).toContain("/audit?limit=500");
  });

  it("rows without a tier render an em dash", async () => {
    render(<AuditPage />);
    const row = (await screen.findByText("Ross")).closest("tr")!;
    expect(row.children[5].textContent).toBe("—");
  });

  it("shows the empty state when filters match nothing, and clear resets", async () => {
    render(<AuditPage />);
    await screen.findByText("Bob");
    await userEvent.type(screen.getByLabelText("Agent filter"), "nobody");
    expect(screen.getByText("No matching events")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByText("Bob")).toBeInTheDocument();
  });

  it("drawer hides the code span for plain allows", async () => {
    render(<AuditPage />);
    await userEvent.click(await screen.findByText("Ross"));
    const drawer = await screen.findByLabelText("Event detail");
    expect(within(drawer).getByText("allowed")).toBeInTheDocument();
    expect(within(drawer).queryByText("denied_replayed")).toBeNull();
  });

  it("metering row without last_seen shows em dash", async () => {
    env.meteringRows = [
      { resource_id: "r1", resource_type: "rest", name: "api", metered: true, calls: 1, cost: 0.01 },
    ];
    render(<AuditPage />);
    await userEvent.click(screen.getByRole("button", { name: "Resource metering" }));
    const table = await screen.findByRole("table");
    const row = within(table).getByText("api").closest("tr")!;
    expect(row.children[5].textContent).toBe("—");
  });
});

describe("resource metering tab (S-269 real per-resource API)", () => {
  it("renders real per-resource calls and cost from the metering endpoint", async () => {
    env.meteringRows = [
      {
        resource_id: "res-aaaa-1111",
        resource_type: "rest",
        name: "paid-api",
        metered: true,
        calls: 4,
        cost: 0.2,
        currency: "USD",
        last_seen: "2026-10-16T00:00:00Z",
      },
      {
        resource_id: "res-bbbb-2222",
        resource_type: "skill",
        name: "free-skill",
        metered: false,
        calls: 9,
        cost: 0,
        currency: "USD",
        last_seen: "2026-10-15T00:00:00Z",
      },
    ];
    render(<AuditPage />);
    await screen.findAllByText("Bob");
    await userEvent.click(screen.getByRole("button", { name: "Resource metering" }));
    expect(env.paths.some((p) => p.startsWith("/metering/resources"))).toBe(true);
    const table = await screen.findByRole("table");
    const paidRow = within(table).getByText("paid-api").closest("tr")!;
    const paidCells = [...paidRow.children].map((td) => td.textContent);
    expect(paidCells[2]).toBe("yes"); // metered
    expect(paidCells[3]).toBe("4"); // calls
    expect(paidCells[4]).toBe("USD 0.2000"); // cost — real money, not "n/a"
    const freeRow = within(table).getByText("free-skill").closest("tr")!;
    const freeCells = [...freeRow.children].map((td) => td.textContent);
    expect(freeCells[2]).toBe("no"); // non-metered
    expect(freeCells[3]).toBe("9"); // usage still recorded
    expect(freeCells[4]).toBe("USD 0.0000"); // explicit billed zero
  });

  it("empty metering shows the empty state", async () => {
    env.meteringRows = [];
    render(<AuditPage />);
    await userEvent.click(screen.getByRole("button", { name: "Resource metering" }));
    expect(await screen.findByText("No resource usage")).toBeInTheDocument();
  });

  it("non-admins get the admin-only empty state and never call the endpoint", async () => {
    env.role = "member";
    env.squads = [{ id: "s1", name: "Alpha Squad" }];
    render(<AuditPage />);
    const select = await screen.findByLabelText("Squad filter");
    await userEvent.selectOptions(select, "s1");
    await screen.findByText("Bob");
    await userEvent.click(screen.getByRole("button", { name: "Resource metering" }));
    expect(await screen.findByText("Platform-admin view")).toBeInTheDocument();
    expect(env.paths.some((p) => p.startsWith("/metering/resources"))).toBe(false);
  });
});

describe("owner (non-admin) scoping", () => {
  it("prompts for a squad and never calls the admin endpoint", async () => {
    env.role = "member";
    render(<AuditPage />);
    expect(await screen.findByText("Pick a squad")).toBeInTheDocument();
    expect(env.paths).not.toContain("/audit?limit=200");
  });

  it("uses the squad-scoped endpoint once a squad is selected", async () => {
    env.role = "member";
    render(<AuditPage />);
    const select = await screen.findByLabelText("Squad filter");
    await userEvent.selectOptions(select, "s1");
    expect(env.paths).toContain("/squads/s1/audit?limit=200");
    expect(await screen.findByText("Bob")).toBeInTheDocument();
  });
});

describe("error state", () => {
  it("surfaces the API error", async () => {
    env.auditError = "boom: forbidden";
    render(<AuditPage />);
    expect(await screen.findByText("boom: forbidden")).toBeInTheDocument();
  });
});
