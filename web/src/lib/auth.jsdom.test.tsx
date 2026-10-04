// S-189: jsdom behaviour tests for the auth provider (previously ~22%
// statements / 5% branches). Covers: token-mode bootstrap from
// localStorage, /auth/me success + failure, setToken persistence,
// logout in both modes, OIDC session bootstrap, config fallback, and
// the useAuth guard outside the provider.
import { renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TokenProvider, useAuth, type AuthMode } from "./auth";
import { setApiBaseOverride } from "./api";

const TOKEN_KEY = "***";

function json(body: unknown, ok = true) {
  return { ok, status: ok ? 200 : 500, json: async () => body };
}

function route(routes: Record<string, ReturnType<typeof json> | Error | string>) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    for (const [key, value] of Object.entries(routes)) {
      if (url.includes(key)) {
        if (typeof value === "string") throw value; // non-Error rejection
        if (value instanceof Error) throw value;
        return value;
      }
    }
    return json({ error: `unmatched ${url}` }, false);
  });
}

function wrapper({ children }: { readonly children: React.ReactNode }) {
  return <TokenProvider>{children}</TokenProvider>;
}

beforeEach(() => {
  window.localStorage.clear();
  setApiBaseOverride(null);
});

afterEach(() => {
  vi.unstubAllGlobals();
  setApiBaseOverride(null);
});

describe("TokenProvider — token mode", () => {
  it("bootstraps a saved token and resolves the user via /auth/me", async () => {
    window.localStorage.setItem(TOKEN_KEY, "saved-tok");
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "token" }),
        "/auth/me": json({ id: "u1", name: "Ross", role: "platform_admin" }),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.user?.name).toBe("Ross"));
    expect(result.current.mode).toBe("token");
    expect(result.current.token).toBe("saved-tok");
    expect(result.current.authed).toBe(true);
    expect(result.current.loading).toBe(false);
  });

  it("falls back to token mode when /auth/config is unreachable", async () => {
    vi.stubGlobal("fetch", route({ "/auth/config": new Error("down") }));
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.mode).toBe("token");
    expect(result.current.authed).toBe(false);
  });

  it("treats a non-ok /auth/config as token mode", async () => {
    vi.stubGlobal("fetch", route({ "/auth/config": json({}, false) }));
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.mode).toBe("token");
  });

  it("treats an unknown config mode as token mode", async () => {
    vi.stubGlobal("fetch", route({ "/auth/config": json({ mode: "magic" }) }));
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.mode).toBe("token");
  });

  it("maps a non-Error /auth/me throw to 'authentication failed'", async () => {
    window.localStorage.setItem(TOKEN_KEY, "stale");
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "token" }),
        "/auth/me": "weird-failure",
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.error).toBe("authentication failed"));
    expect(result.current.user).toBeNull();
  });

  it("surfaces /auth/me failures as an error with no user", async () => {
    window.localStorage.setItem(TOKEN_KEY, "stale");
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "token" }),
        "/auth/me": new Error("401 unauthorized"),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.error).toBe("401 unauthorized"));
    expect(result.current.user).toBeNull();
  });

  it("setToken persists to localStorage and re-resolves the user", async () => {
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "token" }),
        "/auth/me": json({ id: "u2" }),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.loading).toBe(false));
    result.current.setToken("fresh-tok");
    expect(window.localStorage.getItem(TOKEN_KEY)).toBe("fresh-tok");
    await waitFor(() => expect(result.current.user?.id).toBe("u2"));
  });

  it("logout clears the token, storage and user", async () => {
    window.localStorage.setItem(TOKEN_KEY, "bye");
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "token" }),
        "/auth/me": json({ id: "u1" }),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.user?.id).toBe("u1"));
    result.current.logout();
    expect(window.localStorage.getItem(TOKEN_KEY)).toBeNull();
    await waitFor(() => expect(result.current.user).toBeNull());
    expect(result.current.token).toBe("");
    expect(result.current.authed).toBe(false);
  });

  it("clears the user immediately when the token is emptied", async () => {
    window.localStorage.setItem(TOKEN_KEY, "x");
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "token" }),
        "/auth/me": json({ id: "u1" }),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.user?.id).toBe("u1"));
    result.current.setToken("");
    await waitFor(() => expect(result.current.user).toBeNull());
  });
});

describe("TokenProvider — OIDC mode", () => {
  it("bootstraps the session and resolves the user without a token", async () => {
    const fetchMock = route({
      "/auth/config": json({ mode: "oidc" }),
      "/auth/session": json({ ok: true }),
      "/auth/me": json({ id: "oidc-1", name: "SSO User" }),
    });
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.user?.name).toBe("SSO User"));
    expect(result.current.mode).toBe<AuthMode>("oidc");
    expect(result.current.token).toBe("");
    expect(result.current.user?.name).toBe("SSO User");
    expect(result.current.authed).toBe(true);
    // API calls go through the /proxy base in OIDC mode.
    const meCall = fetchMock.mock.calls.find((c) => String(c[0]).includes("/auth/me"));
    expect(String(meCall?.[0])).toContain("/proxy/auth/me");
  });

  it("stays signed out when the session check fails", async () => {
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "oidc" }),
        "/auth/session": json({}, false),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => {
      expect(result.current.mode).toBe("oidc");
      expect(result.current.loading).toBe(false);
    });
    expect(result.current.user).toBeNull();
    expect(result.current.authed).toBe(false);
  });

  it("records session network errors", async () => {
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "oidc" }),
        "/auth/session": new Error("dex unreachable"),
        "/auth/me": json({}, false),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.error).toBe("dex unreachable"));
    expect(result.current.user).toBeNull();
  });

  it("maps a non-Error session failure to 'session check failed'", async () => {
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "oidc" }),
        "/auth/session": "plain-string-failure",
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.error).toBe("session check failed"));
  });

  it("logout in OIDC mode navigates to /auth/logout", async () => {
    const assign = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: { href: "", assign, reload: vi.fn() },
    });
    vi.stubGlobal(
      "fetch",
      route({
        "/auth/config": json({ mode: "oidc" }),
        "/auth/session": json({ ok: true }),
        "/auth/me": json({ id: "oidc-1" }),
      }),
    );
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.user?.id).toBe("oidc-1"));
    result.current.logout();
    expect(assign).toHaveBeenCalledWith("/auth/logout");
  });
});

describe("useAuth guard", () => {
  it("throws outside the provider", () => {
    expect(() => renderHook(() => useAuth())).toThrow(
      "useAuth must be used inside TokenProvider",
    );
  });
});
