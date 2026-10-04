// S-189: jsdom behaviour tests for usePromptValidation (previously
// excluded from coverage as "needs a DOM env"). Covers: debounce timing,
// payload shape per mode, stale-response guard, and error clearing.
//
// Note: `waitFor` deadlocks under vi fake timers in this harness, so all
// timer-driven assertions run inside `act` (which flushes microtasks)
// and assert synchronously afterwards.
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const auth = vi.hoisted(() => ({
  token: "tok",
  mode: "token" as "token" | "oidc",
}));

vi.mock("./auth", () => ({
  useAuth: () => auth,
}));

vi.mock("./api", () => ({
  apiPost: vi.fn(),
}));

import { apiPost } from "./api";
import { usePromptValidation } from "./usePromptValidation";

const mockedPost = vi.mocked(apiPost);

beforeEach(() => {
  auth.token = "tok";
  auth.mode = "token";
  mockedPost.mockReset();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

const ok = { valid: true, tokens: 10, soft_warn: 800, hard_cap: 1200 };

describe("usePromptValidation", () => {
  it("validates after the debounce with {scope, content}", async () => {
    mockedPost.mockResolvedValue(ok);
    const { result } = renderHook(() => usePromptValidation("agent", "hello", 50));
    expect(mockedPost).not.toHaveBeenCalled();
    expect(result.current.validating).toBe(true);
    await act(async () => {
      vi.advanceTimersByTime(50);
    });
    expect(mockedPost).toHaveBeenCalledWith("/prompt/validate", "tok", {
      scope: "agent",
      content: "hello",
    });
    expect(result.current.result).toEqual(ok);
    expect(result.current.validating).toBe(false);
  });

  it("sends an empty bearer in OIDC mode", async () => {
    auth.mode = "oidc";
    mockedPost.mockResolvedValue(ok);
    renderHook(() => usePromptValidation("squad", "s", 10));
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(mockedPost).toHaveBeenCalledWith("/prompt/validate", "", {
      scope: "squad",
      content: "s",
    });
  });

  it("restarts the timer on each keystroke (only the latest content validates)", async () => {
    mockedPost.mockResolvedValue(ok);
    const { rerender } = renderHook(
      ({ content }: { content: string }) => usePromptValidation("agent", content, 50),
      { initialProps: { content: "a" } },
    );
    await act(async () => {
      vi.advanceTimersByTime(30);
    });
    rerender({ content: "ab" });
    await act(async () => {
      vi.advanceTimersByTime(30);
    });
    // 60ms elapsed but the 50ms timer was reset at t=30 → nothing yet.
    expect(mockedPost).not.toHaveBeenCalled();
    await act(async () => {
      vi.advanceTimersByTime(20);
    });
    expect(mockedPost).toHaveBeenCalledTimes(1);
    expect(mockedPost).toHaveBeenCalledWith("/prompt/validate", "tok", {
      scope: "agent",
      content: "ab",
    });
  });

  it("drops stale responses so slow old results never clobber new ones", async () => {
    let resolveFirst: (v: unknown) => void = () => undefined;
    mockedPost
      .mockImplementationOnce(
        () => new Promise((r) => (resolveFirst = r)) as Promise<never>,
      )
      .mockResolvedValue({ ...ok, tokens: 99 });

    const { rerender, result } = renderHook(
      ({ content }: { content: string }) => usePromptValidation("agent", content, 10),
      { initialProps: { content: "old" } },
    );
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    rerender({ content: "new" });
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(result.current.result).toEqual({ ...ok, tokens: 99 });
    // The slow first response lands after the newer one was already shown.
    await act(async () => {
      resolveFirst({ ...ok, tokens: 1 });
    });
    expect(result.current.result).toEqual({ ...ok, tokens: 99 });
  });

  it("clears the result when the validation request fails", async () => {
    mockedPost.mockRejectedValueOnce(new Error("network"));
    const { result } = renderHook(() => usePromptValidation("agent", "x", 10));
    expect(result.current.validating).toBe(true);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(result.current.result).toBeNull();
    expect(result.current.validating).toBe(false);
  });
});
