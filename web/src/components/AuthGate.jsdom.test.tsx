// S-189: jsdom behaviour tests for AuthGate (previously 0%). Covers:
// loading state, authenticated passthrough, the token-mode paste form
// (trim + submit + error display), and the OIDC login chooser with the
// break-glass admin flow (status probe, submit, failure message).
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const auth = vi.hoisted(() => ({
  token: "",
  user: null as null | { id: string },
  loading: false,
  error: "",
  mode: "token" as "token" | "oidc",
  setToken: vi.fn(),
  logout: vi.fn(),
}));

vi.mock("../lib/auth", () => ({ useAuth: () => auth }));

let loc: { href: string; assign: ReturnType<typeof vi.fn>; reload: ReturnType<typeof vi.fn> };

beforeEach(() => {
  auth.token = "";
  auth.user = null;
  auth.loading = false;
  auth.error = "";
  auth.mode = "token";
  auth.setToken.mockReset();
  loc = { href: "", assign: vi.fn(), reload: vi.fn() };
  Object.defineProperty(window, "location", {
    configurable: true,
    writable: true,
    value: loc,
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

import { AuthGate } from "./AuthGate";

describe("AuthGate", () => {
  it("shows a loading notice while the session is being checked", () => {
    auth.loading = true;
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    expect(screen.getByText("Checking session…")).toBeInTheDocument();
    expect(screen.queryByText("secret")).not.toBeInTheDocument();
  });

  it("renders children when token-mode auth is present", () => {
    auth.token = "skquad-abc";
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    expect(screen.getByText("secret")).toBeInTheDocument();
  });

  it("hides children when token-mode auth has an error", () => {
    auth.token = "stale";
    auth.error = "401 unauthorized";
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    expect(screen.queryByText("secret")).not.toBeInTheDocument();
    expect(screen.getByText("401 unauthorized")).toBeInTheDocument();
  });

  it("submits the trimmed draft token from the paste form", async () => {
    const user = userEvent.setup();
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    await user.type(screen.getByPlaceholderText("skquad API token"), "  tok-99  ");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(auth.setToken).toHaveBeenCalledWith("tok-99");
  });

  it("OIDC: renders the SSO chooser and navigates to /auth/login", async () => {
    auth.mode = "oidc";
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, json: async () => ({ enabled: false }) }),
    );
    const user = userEvent.setup();
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    expect(screen.getByText("Choose how you want to authenticate.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Break-glass admin" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Continue with GitHub/ }));
    expect(loc.href).toBe("/auth/login");
  });

  it("OIDC: shows break-glass form when enabled and posts credentials", async () => {
    auth.mode = "oidc";
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ enabled: true }) })
      .mockResolvedValueOnce({ ok: true, json: async () => ({}) });
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    await user.click(await screen.findByRole("button", { name: "Break-glass admin" }));
    await user.type(screen.getByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "s3cret");
    await user.click(screen.getByRole("button", { name: "Sign in as break-glass" }));
    await waitFor(() => expect(loc.reload).toHaveBeenCalled());
    const post = fetchMock.mock.calls.find((c) => c[0] === "/auth/breakglass");
    expect(post).toBeDefined();
    expect(post![1]).toMatchObject({
      method: "POST",
      body: JSON.stringify({ username: "admin", password: "s3cret" }),
    });
  });

  it("OIDC: surfaces the server error message on a failed break-glass login", async () => {
    auth.mode = "oidc";
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ enabled: true }) })
      .mockResolvedValueOnce({ ok: false, json: async () => ({ error: "bad credentials" }) });
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    await user.click(await screen.findByRole("button", { name: "Break-glass admin" }));
    await user.type(screen.getByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "nope");
    await user.click(screen.getByRole("button", { name: "Sign in as break-glass" }));
    expect(await screen.findByText("bad credentials")).toBeInTheDocument();
    expect(loc.reload).not.toHaveBeenCalled();
  });

  it("OIDC: break-glass network failure shows a friendly notice", async () => {
    auth.mode = "oidc";
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ enabled: true }) })
      .mockRejectedValueOnce(new Error("down"));
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();
    render(
      <AuthGate>
        <p>secret</p>
      </AuthGate>,
    );
    await user.click(await screen.findByRole("button", { name: "Break-glass admin" }));
    await user.type(screen.getByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "x");
    await user.click(screen.getByRole("button", { name: "Sign in as break-glass" }));
    expect(await screen.findByText("Could not reach the server.")).toBeInTheDocument();
  });
});
