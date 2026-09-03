import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, apiBaseUrl, apiDelete, apiGet, apiPatch, apiPost, apiPut } from "./api";

type FetchCall = { url: string; init: RequestInit };

function stubResponse(options: {
  status?: number;
  ok?: boolean;
  statusText?: string;
  json?: unknown;
  invalidJson?: boolean;
}) {
  const status = options.status ?? 200;
  return {
    ok: options.ok ?? (status >= 200 && status < 300),
    status,
    statusText: options.statusText ?? "OK",
    json: async () => {
      if (options.invalidJson) {
        throw new TypeError("not json");
      }
      return options.json;
    },
  };
}

let calls: FetchCall[] = [];

function lastCall(): FetchCall {
  expect(calls.length).toBeGreaterThan(0);
  return calls[calls.length - 1];
}

beforeEach(() => {
  calls = [];
  vi.stubEnv("NEXT_PUBLIC_SKQUAD_API_BASE_URL", "");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return stubResponse({ json: { ok: true } }) as unknown as Response;
    }),
  );
});

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

describe("apiBaseUrl", () => {
  it("defaults to the same-origin API prefix", () => {
    expect(apiBaseUrl()).toBe("/api/v1");
  });

  it("uses the configured base URL and strips a trailing slash", () => {
    vi.stubEnv("NEXT_PUBLIC_SKQUAD_API_BASE_URL", "https://api.skquad.test/api/v1/");
    expect(apiBaseUrl()).toBe("https://api.skquad.test/api/v1");
  });
});

describe("apiRequest", () => {
  it("sends GET without a body or content type", async () => {
    await expect(apiGet<{ ok: boolean }>("/squads", "token-123")).resolves.toEqual({ ok: true });

    const call = lastCall();
    expect(call.url).toBe("/api/v1/squads");
    expect(call.init.method).toBe("GET");
    expect((call.init.headers as Record<string, string>).Accept).toBe("application/json");
    expect((call.init.headers as Record<string, string>).Authorization).toBe("Bearer token-123");
    expect((call.init.headers as Record<string, string>)["Content-Type"]).toBeUndefined();
    expect(call.init.body).toBeUndefined();
    expect(call.init.credentials).toBe("same-origin");
    expect(call.init.cache).toBe("no-store");
  });

  it("omits the Authorization header when no token is available", async () => {
    await apiGet("/squads", "   ");

    expect((lastCall().init.headers as Record<string, string>).Authorization).toBeUndefined();
  });

  it("trims the token before building the bearer header", async () => {
    await apiGet("/squads", "  spaced-token  ");

    expect((lastCall().init.headers as Record<string, string>).Authorization).toBe("Bearer spaced-token");
  });

  it("serialises JSON bodies for writes", async () => {
    await apiPost("/squads", "token", { name: "alpha" });
    expect(lastCall().init.method).toBe("POST");
    expect((lastCall().init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect(lastCall().init.body).toBe(JSON.stringify({ name: "alpha" }));

    await apiPatch("/tasks/1", "token", { status: "done" });
    expect(lastCall().init.method).toBe("PATCH");

    await apiPut("/agents/1", "token", { status: "idle" });
    expect(lastCall().init.method).toBe("PUT");
  });

  it("returns undefined for 204 responses without parsing a body", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => stubResponse({ status: 204, ok: true }) as unknown as Response),
    );

    await expect(apiDelete("/tasks/1", "token")).resolves.toBeUndefined();
  });

  it("surfaces the server error message from the response body", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        stubResponse({ status: 403, ok: false, statusText: "Forbidden", json: { error: { message: "not allowed" } } }) as unknown as Response),
    );

    const error = await apiGet("/squads", "token").catch((err) => err);
    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).status).toBe(403);
    expect((error as ApiError).message).toBe("not allowed");
  });

  it("falls back to the HTTP status text when the body is not JSON", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => stubResponse({ status: 502, ok: false, statusText: "Bad Gateway", invalidJson: true }) as unknown as Response),
    );

    const error = await apiGet("/squads", "token").catch((err) => err);
    expect((error as ApiError).status).toBe(502);
    expect((error as ApiError).message).toBe("Bad Gateway");
  });

  it("falls back to the status text when the JSON body has no error message", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => stubResponse({ status: 409, ok: false, statusText: "Conflict", json: {} }) as unknown as Response),
    );

    const error = await apiGet("/squads", "token").catch((err) => err);
    expect((error as ApiError).message).toBe("Conflict");
  });
});
