// S-291: jsdom behaviour tests for the Squad Settings page (Board
// Policy). Covers owner-gating, effective-value prefill, the whole-
// document PUT on save, invalid-cap gating and 403/400 error surfacing.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  squads: [] as Record<string, unknown>[],
  policy: null as Record<string, unknown> | null,
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
    });
    expect(env.refreshes.some((p) => p.includes("/board-policy"))).toBe(true);
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
