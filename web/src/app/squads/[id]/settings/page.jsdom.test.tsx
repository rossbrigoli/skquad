// S-291: jsdom behaviour tests for the Squad Settings page (Board
// Policy). Covers owner-gating, effective-value prefill, the whole-
// document PUT on save, invalid-cap gating and 403/400 error surfacing.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  squads: [] as Record<string, unknown>[],
  policy: null as Record<string, unknown> | null,
  agents: [] as Record<string, unknown>[],
  gets: [] as string[],
  puts: [] as { path: string; body: unknown }[],
  putThrow: null as unknown,
  refreshes: [] as string[],
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ id: "sq1" }),
}));

vi.mock("../../../../components/AuthGate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../../../components/AppShell", () => ({
  AppShell: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../../../components/EmptyState", () => ({
  EmptyState: ({ title }: { title: string }) => <div>{title}</div>,
}));

vi.mock("../../../../lib/auth", () => ({
  useAuth: () => ({ user: { id: "u1" }, token: "tok", mode: "token", logout: vi.fn() }),
}));

vi.mock("../../../../lib/api", () => ({
  apiGet: async (path: string) => {
    env.gets.push(path);
    if (path.includes("/board-policy")) return env.policy;
    return {};
  },
  apiPut: async (path: string, _token: string, body: unknown) => {
    env.puts.push({ path, body });
    if (env.putThrow) throw env.putThrow;
    return {};
  },
}));

vi.mock("../../../../lib/useApi", () => ({
  useApi: (path: string) => {
    let data: unknown = null;
    if (path === "/squads") data = env.squads;
    if (path.includes("/board-policy")) data = env.policy;
    if (path.includes("/agents")) data = env.agents;
    return {
      data,
      loading: false,
      error: "",
      refresh: () => env.refreshes.push(path),
    };
  },
}));

import SquadSettingsPage from "./page";

beforeEach(() => {
  env.squads = [{ id: "sq1", name: "Alpha", owner_id: "u1" }];
  env.policy = {
    raw: { reopen_cap_per_day: 5 },
    effective: { reopen_cap_per_day: 5, self_approve_default: true, leader_approval_allowed: false },
  };
  env.agents = [];
  env.gets = [];
  env.puts = [];
  env.putThrow = null;
  env.refreshes = [];
});

describe("SquadSettingsPage board policy", () => {
  it("prefills the form from the effective values", () => {
    render(<SquadSettingsPage />);
    expect(screen.getByLabelText("Reopen cap per day")).toHaveValue("5");
    expect(screen.getByLabelText("Self-approve by default")).toBeChecked();
    expect(screen.getByLabelText("Leader may approve others' tasks")).not.toBeChecked();
    expect(screen.getByText(/Currently effective: 5/)).toBeInTheDocument();
  });

  it("save PUTs the whole document and refreshes", async () => {
    render(<SquadSettingsPage />);
    const save = screen.getByRole("button", { name: "Save board policy" });
    expect(save).toBeDisabled();
    const cap = screen.getByLabelText("Reopen cap per day");
    await userEvent.clear(cap);
    await userEvent.type(cap, "2");
    await userEvent.click(screen.getByLabelText("Self-approve by default"));
    await userEvent.click(screen.getByLabelText("Leader may approve others' tasks"));
    expect(save).toBeEnabled();
    await userEvent.click(save);
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].path).toBe("/squads/sq1/board-policy");
    expect(env.puts[0].body).toEqual({
      reopen_cap_per_day: 2,
      self_approve_default: false,
      leader_approval_allowed: true,
      leader_grants: {},
    });
    expect(env.refreshes.some((p) => p.includes("/board-policy"))).toBe(true);
  });

  it("S-294: save passes raw leader_grants through untouched", async () => {
    env.policy = {
      raw: {
        reopen_cap_per_day: 5,
        leader_grants: { "a-lead": { can_prioritize: true, can_assign_others: true } },
      },
      effective: { reopen_cap_per_day: 5, self_approve_default: true, leader_approval_allowed: false },
    };
    render(<SquadSettingsPage />);
    const cap = screen.getByLabelText("Reopen cap per day");
    await userEvent.clear(cap);
    await userEvent.type(cap, "4");
    await userEvent.click(screen.getByRole("button", { name: "Save board policy" }));
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].body).toMatchObject({
      reopen_cap_per_day: 4,
      leader_grants: { "a-lead": { can_prioritize: true, can_assign_others: true } },
    });
  });

  it("zero reopen cap is valid and savable", async () => {
    render(<SquadSettingsPage />);
    const cap = screen.getByLabelText("Reopen cap per day");
    await userEvent.clear(cap);
    await userEvent.type(cap, "0");
    const save = screen.getByRole("button", { name: "Save board policy" });
    expect(save).toBeEnabled();
    await userEvent.click(save);
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].body).toMatchObject({ reopen_cap_per_day: 0 });
  });

  it("empty cap disables save with a reason", async () => {
    render(<SquadSettingsPage />);
    await userEvent.clear(screen.getByLabelText("Reopen cap per day"));
    expect(screen.getByRole("button", { name: "Save board policy" })).toBeDisabled();
    expect(screen.getByText("Reopen cap is invalid.")).toBeInTheDocument();
  });

  it("non-digit input is stripped (no negative caps)", async () => {
    render(<SquadSettingsPage />);
    const cap = screen.getByLabelText("Reopen cap per day");
    await userEvent.clear(cap);
    await userEvent.type(cap, "-3");
    expect(cap).toHaveValue("3");
  });

  it("save failure surfaces the error alert (403/400)", async () => {
    env.putThrow = new Error("403: you are not the squad owner");
    render(<SquadSettingsPage />);
    const cap = screen.getByLabelText("Reopen cap per day");
    await userEvent.clear(cap);
    await userEvent.type(cap, "9");
    await userEvent.click(screen.getByRole("button", { name: "Save board policy" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not the squad owner");
  });

  it("non-owner sees the gate notice and no form", () => {
    env.squads = [{ id: "sq1", name: "Alpha", owner_id: "someone-else" }];
    render(<SquadSettingsPage />);
    expect(screen.getByText(/Only the squad owner can view and edit board policy/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save board policy" })).not.toBeInTheDocument();
  });

  it("missing squad renders the empty state", () => {
    env.squads = [];
    render(<SquadSettingsPage />);
    expect(screen.getByText("Squad not found")).toBeInTheDocument();
  });
});

describe("SquadSettingsPage leader grants (S-297)", () => {
  const agents = [
    { id: "a-lead", squad_id: "sq1", name: "Lead", role: "coordinator" },
    { id: "a-work", squad_id: "sq1", name: "Worker", role: "builder" },
  ];

  it("renders toggles from board_policy state with leader badge and defaults OFF", () => {
    env.agents = agents;
    env.policy = {
      raw: { reopen_cap_per_day: 5, leader_grants: { "a-lead": { can_prioritize: true, can_assign_others: false } } },
      effective: { reopen_cap_per_day: 5, self_approve_default: true, leader_approval_allowed: false },
    };
    render(<SquadSettingsPage />);
    expect(screen.getByLabelText("Lead can prioritize")).toBeChecked();
    expect(screen.getByLabelText("Lead can assign others")).not.toBeChecked();
    // Default OFF for agents with no grant entry.
    expect(screen.getByLabelText("Worker can prioritize")).not.toBeChecked();
    expect(screen.getByLabelText("Worker can assign others")).not.toBeChecked();
    // Leader-capable badge only on the grant holder.
    expect(screen.getAllByText("• leader")).toHaveLength(1);
    expect(screen.queryByText(/Worker • leader/)).not.toBeInTheDocument();
    // Save disabled when nothing changed.
    expect(screen.getByRole("button", { name: "Save board policy" })).toBeDisabled();
  });

  it("toggling a grant marks the form dirty and enables save", async () => {
    env.agents = agents;
    render(<SquadSettingsPage />);
    const save = screen.getByRole("button", { name: "Save board policy" });
    expect(save).toBeDisabled();
    await userEvent.click(screen.getByLabelText("Worker can assign others"));
    expect(save).toBeEnabled();
    expect(screen.getByText("Unsaved changes.")).toBeInTheDocument();
  });

  it("save sends edited leader_grants without dropping other board_policy keys", async () => {
    env.agents = agents;
    env.policy = {
      raw: {
        reopen_cap_per_day: 5,
        self_approve_default: true,
        leader_approval_allowed: false,
        some_future_policy_key: { keep: "me" },
        leader_grants: { "a-lead": { can_prioritize: true, can_assign_others: true } },
      },
      effective: { reopen_cap_per_day: 5, self_approve_default: true, leader_approval_allowed: false },
    };
    render(<SquadSettingsPage />);
    // Revoke Lead's prioritize, grant Worker prioritize.
    await userEvent.click(screen.getByLabelText("Lead can prioritize"));
    await userEvent.click(screen.getByLabelText("Worker can prioritize"));
    await userEvent.click(screen.getByRole("button", { name: "Save board policy" }));
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].body).toEqual({
      reopen_cap_per_day: 5,
      self_approve_default: true,
      leader_approval_allowed: false,
      some_future_policy_key: { keep: "me" },
      // Lead keeps can_assign_others (only prioritize was toggled off).
      leader_grants: {
        "a-lead": { can_prioritize: false, can_assign_others: true },
        "a-work": { can_prioritize: true, can_assign_others: false },
      },
    });
  });

  it("save re-fetches the policy fresh before the PUT (read-modify-write)", async () => {
    env.agents = agents;
    render(<SquadSettingsPage />);
    // useApi is mocked, so the only apiGet call comes from save()'s
    // explicit fresh read before the whole-document PUT.
    expect(env.gets).toHaveLength(0);
    await userEvent.click(screen.getByLabelText("Worker can prioritize"));
    await userEvent.click(screen.getByRole("button", { name: "Save board policy" }));
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.gets).toEqual(["/squads/sq1/board-policy"]);
  });

  it("leader approval knob defaults OFF when effective is false", () => {
    env.agents = agents;
    render(<SquadSettingsPage />);
    expect(screen.getByLabelText("Leader may approve others' tasks")).not.toBeChecked();
  });

  it("turning both grants off revokes the agent on save", async () => {
    env.agents = agents;
    env.policy = {
      raw: { reopen_cap_per_day: 5, leader_grants: { "a-lead": { can_prioritize: true, can_assign_others: true } } },
      effective: { reopen_cap_per_day: 5, self_approve_default: true, leader_approval_allowed: false },
    };
    render(<SquadSettingsPage />);
    await userEvent.click(screen.getByLabelText("Lead can prioritize"));
    await userEvent.click(screen.getByLabelText("Lead can assign others"));
    await userEvent.click(screen.getByRole("button", { name: "Save board policy" }));
    await vi.waitFor(() => expect(env.puts).toHaveLength(1));
    expect(env.puts[0].body).toMatchObject({ leader_grants: {} });
  });

  it("non-owner never sees the grants table", () => {
    env.agents = agents;
    env.squads = [{ id: "sq1", name: "Alpha", owner_id: "someone-else" }];
    render(<SquadSettingsPage />);
    expect(screen.queryByLabelText("Lead can prioritize")).not.toBeInTheDocument();
  });
});
