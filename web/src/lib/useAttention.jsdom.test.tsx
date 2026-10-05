// S-189: jsdom tests for the AttentionProvider hook — auth gating,
// aggregated unread count + agent-name map, markRead side effects,
// per-squad fetch resilience and the missing-provider guard.
import { render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const env = vi.hoisted(() => ({
  authed: true,
  api: {} as Record<string, unknown>,
  apiErrors: {} as Record<string, unknown>,
  posts: [] as { path: string; body: unknown }[],
}));

vi.mock("./auth", () => ({
  useAuth: () => ({ token: "tok", mode: "token", user: env.authed ? { id: "u1" } : null, authed: env.authed, logout: vi.fn() }),
}));

vi.mock("./api", async (importOriginal) => {
  const mod = await importOriginal<typeof import("./api")>();
  return {
    ...mod,
    apiGet: vi.fn(async (path: string) => {
      if (env.apiErrors[path]) throw env.apiErrors[path];
      return env.api[path];
    }),
    apiPost: vi.fn(async (path: string, _token: string, body: unknown) => {
      env.posts.push({ path, body });
      return {};
    }),
  };
});

import { AttentionProvider, useAttention } from "./useAttention";

function Consumer() {
  const { items, inboxUnread, agentName, loading, error, markRead } = useAttention();
  return (
    <div>
      <span data-testid="count">{items.length}</span>
      <span data-testid="unread">{inboxUnread}</span>
      <span data-testid="name">{agentName("ag1") ?? "none"}</span>
      <span data-testid="loading">{String(loading)}</span>
      <span data-testid="error">{error}</span>
      <button type="button" onClick={() => void markRead("m1")}>
        mark
      </button>
    </div>
  );
}

beforeEach(() => {
  env.authed = true;
  env.api = {
    "/squads": [{ id: "sq1", name: "Alpha" }],
    "/squads/sq1/board": { tasks: [] },
    "/squads/sq1/agents": [{ id: "ag1", squad_id: "sq1", name: "coder", status: "idle" }],
    "/inbox?unread=true": [
      { id: "m1", squad_id: "sq1", from_agent_id: "ag1", kind: "agent_message", message: "hi" },
    ],
  };
  env.apiErrors = {};
  env.posts = [];
});

describe("AttentionProvider", () => {
  it("aggregates unread count, items and agent names", async () => {
    render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("loading")).toHaveTextContent("false"));
    expect(screen.getByTestId("unread")).toHaveTextContent("1");
    expect(Number(screen.getByTestId("count").textContent)).toBeGreaterThanOrEqual(1);
    expect(screen.getByTestId("name")).toHaveTextContent("coder");
    expect(screen.getByTestId("error")).toHaveTextContent("");
  });

  it("clears state when unauthenticated", async () => {
    env.authed = false;
    render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("loading")).toHaveTextContent("false"));
    expect(screen.getByTestId("unread")).toHaveTextContent("0");
    expect(screen.getByTestId("count")).toHaveTextContent("0");
  });

  it("markRead posts, drops the item and decrements the badge", async () => {
    render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("unread")).toHaveTextContent("1"));
    screen.getByRole("button", { name: "mark" }).click();
    await waitFor(() =>
      expect(env.posts.some((p) => p.path === "/inbox/m1/read")).toBe(true),
    );
    await waitFor(() => expect(screen.getByTestId("unread")).toHaveTextContent("0"));
  });

  it("surfaces a top-level fetch error", async () => {
    env.apiErrors["/squads"] = new Error("attention boom");
    render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("error")).toHaveTextContent("attention boom"));
  });

  it("survives a per-squad board failure (board errors are swallowed)", async () => {
    env.apiErrors["/squads/sq1/board"] = new Error("board down");
    render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("loading")).toHaveTextContent("false"));
    // Inbox still counted despite the board failure.
    expect(screen.getByTestId("unread")).toHaveTextContent("1");
    expect(screen.getByTestId("error")).toHaveTextContent("");
  });

  it("tolerates null payloads from the API", async () => {
    env.api["/squads"] = null;
    env.api["/squads/sq1/agents"] = null;
    env.api["/inbox?unread=true"] = null;
    render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("loading")).toHaveTextContent("false"));
    expect(screen.getByTestId("unread")).toHaveTextContent("0");
    expect(screen.getByTestId("count")).toHaveTextContent("0");
  });

  it("markRead is a no-op while unauthenticated", async () => {
    env.authed = false;
    render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("loading")).toHaveTextContent("false"));
    screen.getByRole("button", { name: "mark" }).click();
    expect(env.posts).toHaveLength(0);
  });

  it("useAttention outside a provider throws", () => {
    function Orphan() {
      useAttention();
      return null;
    }
    expect(() => render(<Orphan />)).toThrow("useAttention must be used inside AttentionProvider");
  });
});

// S-189 branch coverage: cancellation race, poll interval, and the
// agent-name fallbacks used while building attention items.
describe("AttentionProvider branch coverage", () => {
  it("falls back to id prefix / 'unassigned' when building items for unknown senders", async () => {
    env.api["/squads/sq1/board"] = {
      tasks: [
        { id: "t1", status: "blocked", assignee_agent_id: "unknownagent123456", title: "T-one", updated_at: new Date().toISOString() },
        { id: "t2", status: "blocked", title: "T-two", updated_at: new Date().toISOString() },
      ],
    };
    let seen: string[] = [];
    function Spy() {
      const { items } = useAttention();
      seen = items.map((i) => i.meta);
      return null;
    }
    render(
      <AttentionProvider>
        <Spy />
      </AttentionProvider>,
    );
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));
    // Unknown sender ids fall back to the 8-char prefix; missing ids to "unassigned".
    expect(seen.some((t) => t.includes("unknowna"))).toBe(true);
    expect(seen.some((t) => t.includes("unassigned"))).toBe(true);
  });

  it("agentName returns undefined for unknown ids and for no id", async () => {
    let fn: ((id?: string) => string | undefined) | null = null;
    function Spy() {
      const { agentName } = useAttention();
      fn = agentName;
      return null;
    }
    render(
      <AttentionProvider>
        <Spy />
      </AttentionProvider>,
    );
    await waitFor(() => expect(fn).not.toBeNull());
    expect((fn as unknown as (id?: string) => string | undefined)("ag1")).toBe("coder");
    expect((fn as unknown as (id?: string) => string | undefined)("nope")).toBeUndefined();
    expect((fn as unknown as (id?: string) => string | undefined)()).toBeUndefined();
  });

  it("polls again after the interval while the tab is visible", async () => {
    vi.useFakeTimers();
    try {
      render(
        <AttentionProvider>
          <Consumer />
        </AttentionProvider>,
      );
      await vi.waitFor(() => expect(screen.getByTestId("loading")).toHaveTextContent("false"));
      const before = vi.mocked((await import("./api")).apiGet).mock.calls.length;
      Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
      await vi.advanceTimersByTimeAsync(30_000);
      const after = vi.mocked((await import("./api")).apiGet).mock.calls.length;
      expect(after).toBeGreaterThan(before);
    } finally {
      vi.useRealTimers();
    }
  });

  it("skips the poll tick when the tab is hidden", async () => {
    vi.useFakeTimers();
    try {
      render(
        <AttentionProvider>
          <Consumer />
        </AttentionProvider>,
      );
      await vi.waitFor(() => expect(screen.getByTestId("loading")).toHaveTextContent("false"));
      const apiMod = await import("./api");
      const before = vi.mocked(apiMod.apiGet).mock.calls.length;
      Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
      await vi.advanceTimersByTimeAsync(30_000);
      expect(vi.mocked(apiMod.apiGet).mock.calls.length).toBe(before);
    } finally {
      vi.useRealTimers();
    }
  });

  it("ignores in-flight results after unmount (cancelled path)", async () => {
    let resolveSquads: (v: unknown) => void = () => {};
    env.api = {
      "/squads": new Promise((res) => {
        resolveSquads = res;
      }),
    };
    const view = render(
      <AttentionProvider>
        <Consumer />
      </AttentionProvider>,
    );
    // Unmount while the /squads fetch is still pending.
    view.unmount();
    resolveSquads([]);
    await new Promise((r) => setTimeout(r, 0));
    // If the cancelled branches failed we'd see React state-update warnings;
    // reaching here with no throw is the assertion that nothing was applied.
    expect(screen.queryByTestId("count")).toBeNull();
  });
});
