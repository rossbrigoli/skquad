// TG-8 coverage top-up: defensive + detail branches of the grant-requests
// view that the main suite never reaches — fetch-level error notice,
// non-array payload guard, collapse-on-second-toggle, blank-reason deny
// fallback, null agent display, no-scope fallback, denial reason and the
// owner/admin approval stamps, and the non-Error rejection fallback.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  role: "member" as string,
  byPath: {} as Record<string, unknown>,
  listError: "",
  approveOwner: vi.fn(),
  approveAdmin: vi.fn(),
  deny: vi.fn(),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1", role: env.role } }),
}));

vi.mock("../lib/useApi", () => ({
  useApi: (path: string) => ({
    data: env.byPath[path],
    loading: false,
    error: env.listError,
    refresh: vi.fn(),
  }),
}));

vi.mock("../lib/grantsApi", () => ({
  approveGrantOwner: (...args: unknown[]) => env.approveOwner(...args),
  approveGrantAdmin: (...args: unknown[]) => env.approveAdmin(...args),
  denyGrantRequest: (...args: unknown[]) => env.deny(...args),
}));

import { GrantRequestsPanel } from "./GrantRequestsPanel";

const OWNER_PATH = "/grant-requests?mine=owner";

function req(id: string, overrides: Record<string, unknown> = {}) {
  return {
    id,
    resource_id: `res-${id}`,
    agent_id: "ag1",
    requester_user_id: "u2",
    tier: "high",
    state: "pending_owner",
    requested_scope: { hosts: ["api.example.com"] },
    findings: [],
    created_at: "2026-10-07T00:00:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  env.role = "member";
  env.byPath = {};
  env.listError = "";
  env.approveOwner = vi.fn().mockResolvedValue({});
  env.approveAdmin = vi.fn().mockResolvedValue({});
  env.deny = vi.fn().mockResolvedValue({});
});

describe("fetch-level guards", () => {
  it("shows the fetch error notice alongside the section", () => {
    env.listError = "upstream 500";
    env.byPath[OWNER_PATH] = [];
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.getByText("upstream 500")).toBeInTheDocument();
    expect(screen.getByText("No grant requests here")).toBeInTheDocument();
  });

  it("treats a non-array payload as empty", () => {
    env.byPath[OWNER_PATH] = "junk";
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.getByText("No grant requests here")).toBeInTheDocument();
  });
});

describe("row detail branches", () => {
  it("collapses an expanded row on the second toggle", async () => {
    env.byPath[OWNER_PATH] = [req("r1")];
    render(<GrantRequestsPanel isAdmin={false} />);
    const toggle = screen.getByRole("button", { name: /res-r1/ });
    await userEvent.click(toggle);
    expect(screen.getByText("Hosts")).toBeInTheDocument();
    await userEvent.click(toggle);
    expect(screen.queryByText("Hosts")).not.toBeInTheDocument();
  });

  it("renders 'squad grant' when agent_id is null", () => {
    env.byPath[OWNER_PATH] = [req("r1", { agent_id: null })];
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.getByText(/squad grant/)).toBeInTheDocument();
  });

  it("shows denial reason and the default-scope fallback on a denied row", async () => {
    env.byPath[OWNER_PATH] = [
      req("d1", { state: "denied", denied_reason: "policy", requested_scope: {} }),
    ];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: /res-d1/ }));
    expect(screen.getByText("Denied: policy")).toBeInTheDocument();
    expect(screen.getByText("Requested scope: default (no overrides).")).toBeInTheDocument();
  });

  it("shows owner + admin approval stamps on an approved row", async () => {
    env.byPath[OWNER_PATH] = [
      req("a1", {
        state: "approved",
        requested_scope: null,
        approved_by_owner_at: "2026-10-06T10:00:00Z",
        approved_by_admin_at: "2026-10-06T11:00:00Z",
      }),
    ];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: /res-a1/ }));
    expect(screen.getByText("Owner approved: 2026-10-06T10:00:00Z")).toBeInTheDocument();
    expect(screen.getByText("Admin approved: 2026-10-06T11:00:00Z")).toBeInTheDocument();
  });
});

describe("action fallbacks", () => {
  it("deny with a blank reason posts the 'denied' fallback", async () => {
    env.byPath[OWNER_PATH] = [req("r1", { state: "pending_owner" })];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: "Deny" }));
    await screen.findByLabelText("Denial reason");
    await userEvent.click(screen.getByRole("button", { name: "Confirm Deny" }));
    await waitFor(() => expect(env.deny).toHaveBeenCalledWith("tok", "r1", "denied"));
  });

  it("falls back to the generic message when the rejection is not an Error", async () => {
    env.approveOwner = vi.fn().mockRejectedValue("boom");
    env.byPath[OWNER_PATH] = [req("r1", { state: "pending_owner" })];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("grant request action failed");
  });
});
