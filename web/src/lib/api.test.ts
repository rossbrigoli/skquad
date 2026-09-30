import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  apiBaseUrl,
  apiDelete,
  apiGet,
  apiPatch,
  apiPost,
  apiPut,
  setApiBaseOverride,
} from "./api";

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
}

afterEach(() => {
  vi.restoreAllMocks();
  setApiBaseOverride(null);
  delete process.env.NEXT_PUBLIC_SKQUAD_API_BASE_URL;
});

describe("apiBaseUrl", () => {
  it("uses the default API path", () => {
    expect(apiBaseUrl()).toBe("/api/v1");
  });

  it("trims trailing slashes from env and override values", () => {
    process.env.NEXT_PUBLIC_SKQUAD_API_BASE_URL = "https://api.example.test/";
    expect(apiBaseUrl()).toBe("https://api.example.test");

    setApiBaseOverride("/proxy/");
    expect(apiBaseUrl()).toBe("/proxy");
  });
});

describe("api request helpers", () => {
  it("sends GET requests with a trimmed bearer token", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse({ ok: true }));

    const out = await apiGet<{ ok: boolean }>("/squads", " token ");

    expect(out).toEqual({ ok: true });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/squads",
      expect.objectContaining({
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "application/json", Authorization: "Bearer token" },
        body: undefined,
      }),
    );
  });

  it("serializes POST, PUT, and PATCH bodies as JSON", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async () => jsonResponse({ id: "x" }));

    await apiPost("/items", "", { name: "n" });
    await apiPut("/items/x", "", { name: "p" });
    await apiPatch("/items/x", "", { name: "q" });

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/api/v1/items",
      expect.objectContaining({
        method: "POST",
        headers: { Accept: "application/json", "Content-Type": "application/json" },
        body: JSON.stringify({ name: "n" }),
      }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/api/v1/items/x",
      expect.objectContaining({ method: "PUT", body: JSON.stringify({ name: "p" }) }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      3,
      "/api/v1/items/x",
      expect.objectContaining({ method: "PATCH", body: JSON.stringify({ name: "q" }) }),
    );
  });

  it("returns undefined for DELETE 204 responses", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 204 }));

    await expect(apiDelete("/items/x", "tok")).resolves.toBeUndefined();
  });

  it("throws ApiError with nested JSON error messages", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse({ error: { message: "nope" }, usage: ["agent-1"] }, { status: 409, statusText: "Conflict" }),
    );

    await expect(apiGet("/items/x", "tok")).rejects.toMatchObject({
      status: 409,
      message: "nope",
      body: { error: { message: "nope" }, usage: ["agent-1"] },
    } satisfies Partial<ApiError>);
  });

  it("uses top-level JSON messages or HTTP status text on errors", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ message: "bad request" }, { status: 400, statusText: "Bad Request" }))
      .mockResolvedValueOnce(new Response("not json", { status: 502, statusText: "Bad Gateway" }));

    await expect(apiGet("/bad", "")).rejects.toThrow("bad request");
    await expect(apiGet("/gateway", "")).rejects.toThrow("Bad Gateway");
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
