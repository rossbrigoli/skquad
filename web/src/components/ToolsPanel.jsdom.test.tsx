// S-241: jsdom behaviour tests for the Tools panel tile toggle —
// the PATCH payload, optimistic flip, pending-disabled switch, revert +
// error notice on failure, and the registry-tile restriction.
import React from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  builtins: { tools: [{ name: "exec", enabled: true, policy: {}, updatedAt: "2026-10-01T00:00:00Z", updatedBy: "ross" }] } as unknown,
  registry: [
    { id: "rt-1", name: "Jira", description: "Atlassian Jira", status: "active" },
  ] as unknown,
  refresh: null as (() => void) | null,
  apiPatch: null as ((...args: unknown[]) => Promise<unknown>) | null,
}));

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));

vi.mock("../lib/auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: { id: "u1" }, logout: vi.fn() }),
}));

vi.mock("../lib/useApi", () => ({
  useApi: (path: string) =>
    path.includes("admin")
      ? { data: env.builtins, loading: false, error: "", refresh: () => env.refresh?.() }
      : { data: env.registry, loading: false, error: "", refresh: vi.fn() },
}));

vi.mock("../lib/api", async (importOriginal) => {
  const mod = await importOriginal<typeof import("../lib/api")>();
  return {
    ...mod,
    apiPatch: (...args: unknown[]) => (env.apiPatch ? env.apiPatch(...args) : Promise.reject(new Error("unset"))),
  };
});

import { ToolsPanel } from "./ToolsPanel";

beforeEach(() => {
  env.refresh = vi.fn();
  env.apiPatch = vi.fn().mockResolvedValue({});
});

describe("ToolsPanel tile toggle (S-241)", () => {
  it("toggle click PATCHes /admin/tools/{name} with {enabled} and refreshes", async () => {
    const refresh = vi.fn();
    env.refresh = refresh;
    render(<ToolsPanel isAdmin />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("switch", { name: "Disable exec" }));
    await vi.waitFor(() =>
      expect(env.apiPatch).toHaveBeenCalledWith("/admin/tools/exec", "tok", { enabled: false }),
    );
    expect(refresh).toHaveBeenCalled();
    // Optimistic flip is visible immediately after the PATCH resolves.
    expect(screen.getByRole("switch", { name: "Enable exec" })).toBeEnabled();
  });

  it("switch is disabled while the PATCH is in flight", async () => {
    let resolvePatch: (v: unknown) => void = () => {};
    env.apiPatch = vi.fn().mockReturnValue(
      new Promise((resolve) => {
        resolvePatch = resolve;
      }),
    );
    render(<ToolsPanel isAdmin />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("switch", { name: "Disable exec" }));
    expect(screen.getByRole("switch", { name: /exec/ })).toBeDisabled();
    resolvePatch({});
    await vi.waitFor(() =>
      expect(screen.getByRole("switch", { name: "Enable exec" })).toBeEnabled(),
    );
  });

  it("failed PATCH reverts the optimistic flip and shows an error notice", async () => {
    env.apiPatch = vi.fn().mockRejectedValue(new Error("boom"));
    render(<ToolsPanel isAdmin />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("switch", { name: "Disable exec" }));
    await vi.waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Could not disable exec: boom",
      ),
    );
    // Reverted: still enabled, switch labelled "Disable exec" again.
    expect(screen.getByRole("switch", { name: "Disable exec" })).toBeEnabled();
  });

  it("registry tile switches never trigger a PATCH", async () => {
    render(<ToolsPanel isAdmin />);
    const jiraSwitch = screen.getByRole("switch", { name: /Jira/ });
    expect(jiraSwitch).toBeDisabled();
    const user = userEvent.setup();
    await user.click(jiraSwitch);
    expect(env.apiPatch).not.toHaveBeenCalled();
  });

  it("non-admins see the registry catalog with inert switches (no built-in row)", () => {
    render(<ToolsPanel isAdmin={false} />);
    expect(screen.queryByRole("switch", { name: /exec/ })).not.toBeInTheDocument();
    expect(screen.getByRole("switch", { name: /Jira/ })).toBeDisabled();
  });

  it("a filter with no matches shows the empty state", async () => {
    render(<ToolsPanel isAdmin />);
    const user = userEvent.setup();
    await user.type(screen.getByRole("searchbox", { name: "Search tools" }), "zzzz");
    expect(screen.getByText(/No tools match/)).toBeInTheDocument();
  });

  it("an empty registry shows the register-one hint", () => {
    env.registry = [];
    env.builtins = { tools: [] };
    render(<ToolsPanel isAdmin />);
    expect(screen.getByText("No tools registered")).toBeInTheDocument();
  });
});
