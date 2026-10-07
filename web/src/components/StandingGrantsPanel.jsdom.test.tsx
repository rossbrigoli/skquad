// TG-8 slice D: jsdom behaviour tests for the Standing Grants panel.
// Locks: table columns, live/expired/revoked visual states, the
// confirm-gated revoke flow (DELETE via the client), and the empty state.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  grants: [] as unknown[],
  error: "",
  loading: false,
  revoke: vi.fn(),
  refresh: vi.fn(),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1", role: "member" } }),
}));

vi.mock("../lib/useApi", () => ({
  useApi: () => ({
    data: env.grants,
    loading: env.loading,
    error: env.error,
    refresh: env.refresh,
  }),
}));

vi.mock("../lib/grantsApi", () => ({
  revokeStandingGrant: (...args: unknown[]) => env.revoke(...args),
}));

import { StandingGrantsPanel } from "./StandingGrantsPanel";

function sg(id: string, overrides: Record<string, unknown> = {}) {
  return {
    id,
    resource_id: "res1",
    agent_id: "ag1",
    tool: "deploy",
    expires_at: "2999-01-01T00:00:00Z",
    created_by: "u1",
    created_at: "2026-10-07T00:00:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  env.grants = [];
  env.error = "";
  env.loading = false;
  env.revoke = vi.fn().mockResolvedValue(undefined);
  env.refresh = vi.fn();
});

describe("table rendering", () => {
  it("renders resource/tool/agent/expiry/created_by columns", () => {
    env.grants = [sg("g1")];
    render(<StandingGrantsPanel />);
    const table = screen.getByRole("table", { name: "Standing grants" });
    expect(table).toBeInTheDocument();
    expect(screen.getByText("res1")).toBeInTheDocument();
    expect(screen.getByText("deploy")).toBeInTheDocument();
    expect(screen.getByText("ag1")).toBeInTheDocument();
    expect(screen.getByText("2999-01-01")).toBeInTheDocument();
  });

  it("live grants get the active chip and a Revoke button", () => {
    env.grants = [sg("g1")];
    render(<StandingGrantsPanel />);
    expect(document.querySelector(".standing-grant-live")).not.toBeNull();
    expect(screen.getByText("active")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Revoke" })).toBeInTheDocument();
  });

  it("expired grants are visually distinct and NOT revocable", () => {
    env.grants = [sg("g2", { expires_at: "2020-01-01T00:00:00Z" })];
    render(<StandingGrantsPanel />);
    expect(document.querySelector(".standing-grant-expired")).not.toBeNull();
    expect(screen.getByText("expired")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke" })).not.toBeInTheDocument();
  });

  it("revoked grants show the revoked chip + revoked-since and no Revoke button", () => {
    env.grants = [sg("g3", { revoked_at: "2026-10-06T09:00:00Z" })];
    render(<StandingGrantsPanel />);
    expect(document.querySelector(".standing-grant-revoked")).not.toBeNull();
    expect(screen.getByText("revoked")).toBeInTheDocument();
    expect(screen.getByText(/since 2026-10-06/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke" })).not.toBeInTheDocument();
  });
});

describe("revoke flow", () => {
  it("Revoke opens a confirm dialog; confirming calls DELETE and refreshes", async () => {
    env.grants = [sg("g1")];
    render(<StandingGrantsPanel />);
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    // Nothing happens until the dialog is confirmed.
    expect(env.revoke).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Revoke standing grant");
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(env.revoke).toHaveBeenCalledWith("tok", "g1"));
    expect(env.refresh).toHaveBeenCalled();
  });

  it("cancelling the dialog does not revoke", async () => {
    env.grants = [sg("g1")];
    render(<StandingGrantsPanel />);
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    await screen.findByRole("dialog");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(env.revoke).not.toHaveBeenCalled();
  });

  it("a failed revoke surfaces an error toast", async () => {
    env.revoke = vi.fn().mockRejectedValue(new Error("404 not found"));
    env.grants = [sg("g1")];
    render(<StandingGrantsPanel />);
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("404 not found");
  });
});

describe("empty + error states", () => {
  it("empty list renders the guiding empty state", () => {
    env.grants = [];
    render(<StandingGrantsPanel />);
    expect(screen.getByText("No standing grants")).toBeInTheDocument();
  });

  it("list fetch error is shown", () => {
    env.error = "boom";
    render(<StandingGrantsPanel />);
    expect(screen.getByText("boom")).toBeInTheDocument();
  });
});

// TG-8 coverage top-up: defensive branches the main suite never hits —
// non-array payloads, missing expiry, and non-Error rejection fallback.
describe("defensive branches (coverage top-up)", () => {
  it("treats a non-array payload as empty", () => {
    env.grants = null as unknown as unknown[];
    render(<StandingGrantsPanel />);
    expect(screen.getByText("No standing grants")).toBeInTheDocument();
  });

  it("shows an em-dash when a grant has no expiry", () => {
    env.grants = [sg("g5", { expires_at: "" })];
    render(<StandingGrantsPanel />);
    expect(screen.getByText("—")).toBeInTheDocument();
  });

  it("falls back to 'revoke failed' when the rejection is not an Error", async () => {
    env.revoke = vi.fn().mockRejectedValue("boom");
    env.grants = [sg("g1")];
    render(<StandingGrantsPanel />);
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("revoke failed");
  });
});
