// S-189: jsdom behaviour tests for useApi (previously excluded from
// coverage as "needs a DOM env"). Covers: empty-path skip, unauthenticated
// gate (token + oidc modes), success, error mapping, visibility-gated
// polling, and refresh().
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const auth = vi.hoisted(() => ({
  token: "tok",
  mode: "token" as "token" | "oidc",
  user: null as { id: string } | null,
}));

vi.mock("./auth", () => ({
  useAuth: () => auth,
}));

vi.mock("./api", () => ({
  apiGet: vi.fn(),
}));

import { apiGet } from "./api";
import { useApi } from "./useApi";

const mockedGet = vi.mocked(apiGet);

beforeEach(() => {
  auth.token = "tok";
  auth.mode = "token";
  auth.user = null;
  mockedGet.mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("useApi", () => {
  it("skips fetching for an empty path without an error", async () => {
    const { result } = renderHook(() => useApi(""));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe("");
    expect(result.current.data).toBeNull();
    expect(mockedGet).not.toHaveBeenCalled();
  });

  it("errors with 'not authenticated' when token mode has no token", async () => {
    auth.token = "";
    const { result } = renderHook(() => useApi<{ ok: boolean }>("/x"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe("not authenticated");
    expect(mockedGet).not.toHaveBeenCalled();
  });

  it("treats OIDC mode as authed via user, fetching with an empty bearer", async () => {
    auth.mode = "oidc";
    auth.token = "";
    auth.user = { id: "u1" };
    mockedGet.mockResolvedValue({ ok: true });
    const { result } = renderHook(() => useApi<{ ok: boolean }>("/me"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.data).toEqual({ ok: true });
    expect(mockedGet).toHaveBeenCalledWith("/me", "");
  });

  it("maps thrown Errors into the error state", async () => {
    mockedGet.mockRejectedValue(new Error("boom 500"));
    const { result } = renderHook(() => useApi("/x"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe("boom 500");
    expect(result.current.data).toBeNull();
  });

  it("maps non-Error throws to 'request failed'", async () => {
    mockedGet.mockRejectedValue("string failure");
    const { result } = renderHook(() => useApi("/x"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe("request failed");
  });

  it("refresh() re-runs the fetch", async () => {
    mockedGet.mockResolvedValue(1).mockResolvedValueOnce(0);
    const { result } = renderHook(() => useApi<number>("/n"));
    await waitFor(() => expect(result.current.data).toBe(0));
    act(() => result.current.refresh());
    await waitFor(() => expect(result.current.data).toBe(1));
    expect(mockedGet).toHaveBeenCalledTimes(2);
  });

  it("polls on an interval only while the document is visible", async () => {
    vi.useFakeTimers();
    mockedGet.mockResolvedValue("v");
    const { result } = renderHook(() => useApi<string>("/poll", 1000));
    await act(async () => {
      vi.advanceTimersByTime(0);
    });
    expect(mockedGet).toHaveBeenCalledTimes(1);

    await act(async () => {
      vi.advanceTimersByTime(1000);
    });
    expect(mockedGet).toHaveBeenCalledTimes(2);

    // Hidden tab: the interval fires but load() is skipped.
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await act(async () => {
      vi.advanceTimersByTime(3000);
    });
    expect(mockedGet).toHaveBeenCalledTimes(2);
  });

  it("clears its timer on unmount", async () => {
    vi.useFakeTimers();
    mockedGet.mockResolvedValue("v");
    const { unmount } = renderHook(() => useApi<string>("/poll", 1000));
    await act(async () => {
      vi.advanceTimersByTime(0);
    });
    const before = mockedGet.mock.calls.length;
    unmount();
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(mockedGet).toHaveBeenCalledTimes(before);
  });
});
