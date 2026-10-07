// TG-8 slice D: jsdom behaviour tests for the grant-request review view.
// Locks: owner vs admin sections, state badges, findings severity styling
// (block=red / warn=amber), role-gated approve buttons (approve-owner vs
// approve-admin) and the deny-with-reason body.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  role: "member" as string,
  byPath: {} as Record<string, unknown>,
  approveOwner: vi.fn(),
  approveAdmin: vi.fn(),
  deny: vi.fn(),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1", role: env.role } }),
}));

vi.mock("../lib/useApi", () => ({
  useApi: (path: string) => ({
    data: env.byPath[path] ?? [],
    loading: false,
    error: "",
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
const ADMIN_PATH = "/grant-requests?mine=admin";

function req(id: string, overrides: Record<string, unknown> = {}) {
  return {
    id,
    resource_id: "res1",
    agent_id: "ag1",
    requester_user_id: "u2",
    tier: "high",
    state: "pending_owner",
    requested_scope: { hosts: ["api.example.com"], http_methods: ["POST"] },
    findings: [],
    created_at: "2026-10-07T00:00:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  env.role = "member";
  env.byPath = {};
  env.approveOwner = vi.fn().mockResolvedValue({});
  env.approveAdmin = vi.fn().mockResolvedValue({});
  env.deny = vi.fn().mockResolvedValue({});
});

describe("sections by role", () => {
  it("member sees only the owner section", () => {
    env.byPath[OWNER_PATH] = [req("r1")];
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.getByText("Requests on resources you own")).toBeInTheDocument();
    expect(screen.queryByText("Admin co-sign queue")).not.toBeInTheDocument();
  });

  it("admin sees both sections", () => {
    env.byPath[OWNER_PATH] = [];
    env.byPath[ADMIN_PATH] = [req("a1", { state: "pending_admin" })];
    render(<GrantRequestsPanel isAdmin />);
    expect(screen.getByText("Requests on resources you own")).toBeInTheDocument();
    expect(screen.getByText("Admin co-sign queue")).toBeInTheDocument();
  });

  it("empty section renders the empty state", () => {
    env.byPath[OWNER_PATH] = [];
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.getByText("No grant requests here")).toBeInTheDocument();
  });
});

describe("row rendering", () => {
  it("shows the state badge and tier/requester meta", () => {
    env.byPath[OWNER_PATH] = [req("r1")];
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.getByText("awaiting owner")).toBeInTheDocument();
    expect(screen.getByText(/tier high/)).toBeInTheDocument();
    expect(screen.getByText(/requested by u2/)).toBeInTheDocument();
  });

  it("expanded row renders findings with severity classes (block red / warn amber)", async () => {
    env.byPath[OWNER_PATH] = [
      req("r1", {
        findings: [
          { code: "metadata_path", severity: "block", detail: "metadata host reachable" },
          { code: "new_http_method", severity: "warn", detail: "DELETE added" },
        ],
      }),
    ];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: /res1/ }));
    const block = document.querySelector(".finding-block");
    const warn = document.querySelector(".finding-warn");
    expect(block).not.toBeNull();
    expect(block).toHaveTextContent("metadata_path");
    expect(block).toHaveTextContent("BLOCK");
    expect(warn).not.toBeNull();
    expect(warn).toHaveTextContent("new_http_method");
    // Row-level summary counts surface on the collapsed meta line.
    expect(screen.getByText(/1 block/)).toBeInTheDocument();
    expect(screen.getByText(/1 warn/)).toBeInTheDocument();
  });

  it("expanded row shows the requested-scope summary", async () => {
    env.byPath[OWNER_PATH] = [req("r1")];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: /res1/ }));
    expect(screen.getByText("Hosts")).toBeInTheDocument();
    expect(screen.getByText("api.example.com")).toBeInTheDocument();
    expect(screen.getByText("HTTP methods")).toBeInTheDocument();
  });
});

describe("role-gated approve buttons", () => {
  it("pending_owner in the owner section → approve-owner, never approve-admin", () => {
    env.byPath[OWNER_PATH] = [req("r1", { state: "pending_owner" })];
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve as admin" })).not.toBeInTheDocument();
  });

  it("pending_owner is NOT admin-approvable — admin sees approve-owner only", () => {
    env.byPath[OWNER_PATH] = [req("r1", { state: "pending_owner" })];
    render(<GrantRequestsPanel isAdmin />);
    expect(screen.getByRole("button", { name: "Approve as owner" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve as admin" })).not.toBeInTheDocument();
  });

  it("pending_admin → approve-admin only for admins; member cannot act at all", () => {
    env.byPath[ADMIN_PATH] = [req("a1", { state: "pending_admin" })];
    render(<GrantRequestsPanel isAdmin />);
    expect(screen.getByRole("button", { name: "Approve as admin" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve as owner" })).not.toBeInTheDocument();
  });

  it("member never sees approve buttons in the admin queue (section absent)", () => {
    env.role = "member";
    render(<GrantRequestsPanel isAdmin={false} />);
    expect(screen.queryByText("Admin co-sign queue")).not.toBeInTheDocument();
  });

  it("terminal states render no action buttons", () => {
    env.byPath[OWNER_PATH] = [
      req("done1", { state: "approved" }),
      req("denied1", { state: "denied", denied_reason: "nope" }),
    ];
    render(<GrantRequestsPanel isAdmin />);
    expect(screen.queryByRole("button", { name: /Approve/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument();
  });
});

describe("approve + deny wiring", () => {
  it("approve-owner button hits approveGrantOwner with the request id", async () => {
    env.byPath[OWNER_PATH] = [req("r1", { state: "pending_owner" })];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(env.approveOwner).toHaveBeenCalledWith("tok", "r1"));
  });

  it("approve-admin button hits approveGrantAdmin", async () => {
    env.byPath[ADMIN_PATH] = [req("a1", { state: "pending_admin" })];
    render(<GrantRequestsPanel isAdmin />);
    await userEvent.click(screen.getByRole("button", { name: "Approve as admin" }));
    await waitFor(() => expect(env.approveAdmin).toHaveBeenCalledWith("tok", "a1"));
  });

  it("deny opens the reason field and posts it", async () => {
    env.byPath[OWNER_PATH] = [req("r1", { state: "pending_owner" })];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: "Deny" }));
    const input = await screen.findByLabelText("Denial reason");
    await userEvent.type(input, "scope too wide");
    await userEvent.click(screen.getByRole("button", { name: "Confirm Deny" }));
    await waitFor(() => expect(env.deny).toHaveBeenCalledWith("tok", "r1", "scope too wide"));
  });

  it("failed action surfaces an error toast", async () => {
    env.approveOwner = vi.fn().mockRejectedValue(new Error("403 forbidden"));
    env.byPath[OWNER_PATH] = [req("r1", { state: "pending_owner" })];
    render(<GrantRequestsPanel isAdmin={false} />);
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("403 forbidden");
  });
});
