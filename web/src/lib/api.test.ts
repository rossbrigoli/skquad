import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  apiBaseUrl,
  apiDelete,
  apiDeleteWithBody,
  apiGet,
  apiGetBlob,
  apiGetWithTotal,
  apiPatch,
  apiPost,
  apiPut,
  apiUploadImage,
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

// S-226: binary GET for image attachments — same auth semantics as
// apiRequest, but returns a Blob instead of parsed JSON.
describe("apiGetBlob", () => {
  it("sends GET with a trimmed bearer token and returns the blob", async () => {
    const pngBytes = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(pngBytes, { headers: { "Content-Type": "image/png" } }));

    const blob = await apiGetBlob("/uploads/abc", " tok ");

    expect(blob.type).toBe("image/png");
    expect(new Uint8Array(await blob.arrayBuffer())).toEqual(pngBytes);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/uploads/abc",
      expect.objectContaining({
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "image/*", Authorization: "Bearer tok" },
      }),
    );
  });

  it("omits the Authorization header when no token (OIDC cookie mode)", async () => {
    setApiBaseOverride("/proxy");
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(new Uint8Array([1, 2, 3]), { headers: { "Content-Type": "image/jpeg" } }));

    const blob = await apiGetBlob("/uploads/xyz", "");

    expect(blob.type).toBe("image/jpeg");
    const call = fetchMock.mock.calls[0];
    expect(call[0]).toBe("/proxy/uploads/xyz");
    const headers = (call[1] as { headers: Record<string, string> }).headers;
    expect(headers.Authorization).toBeUndefined();
    expect(headers.Accept).toBe("image/*");
  });

  it("surfaces the parsed control-plane error message on 401", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse(
        { error: { code: "unauthorized", message: "missing or invalid bearer token" } },
        { status: 401, statusText: "Unauthorized" },
      ),
    );

    await expect(apiGetBlob("/uploads/abc", "")).rejects.toMatchObject({
      status: 401,
      message: "missing or invalid bearer token",
    } satisfies Partial<ApiError>);
  });

  it("falls back to status text when the error body is not JSON", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("boom", { status: 500, statusText: "Server Error" }));
    await expect(apiGetBlob("/uploads/abc", "tok")).rejects.toThrow("Server Error");
  });
});

// S-189 branch coverage: apiDeleteWithBody, apiUploadImage, and the
// top-level `message` fallback in extractErrorMessage.
describe("apiDeleteWithBody", () => {
  it("sends DELETE with a JSON body and trimmed bearer token", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse({ deleted: 2 }));

    const out = await apiDeleteWithBody<{ deleted: number }>("/cards/bulk", " tok ", { ids: ["a", "b"] });

    expect(out).toEqual({ deleted: 2 });
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(call[0]).toBe("/api/v1/cards/bulk");
    expect(call[1].method).toBe("DELETE");
    expect(call[1].headers).toMatchObject({ "Content-Type": "application/json", Authorization: "Bearer tok" });
    expect(JSON.parse(String(call[1].body))).toEqual({ ids: ["a", "b"] });
  });

  it("omits Authorization when token is empty", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse({ ok: true }));
    await apiDeleteWithBody("/x", "", {});
    const headers = (fetchMock.mock.calls[0] as [string, RequestInit])[1].headers as Record<string, string>;
    expect(headers.Authorization).toBeUndefined();
  });
});

describe("apiGetWithTotal", () => {
  it("returns items plus the X-Total-Count header", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(
        jsonResponse([1, 2], { headers: { "Content-Type": "application/json", "X-Total-Count": "42" } }),
      );

    const out = await apiGetWithTotal<number[]>("/inbox?limit=2", "tok");

    expect(out).toEqual({ items: [1, 2], total: 42 });
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(call[0]).toBe("/api/v1/inbox?limit=2");
    expect(call[1].method).toBe("GET");
    expect(call[1].headers).toMatchObject({ Authorization: "Bearer tok" });
  });

  it("omits Authorization when the token is blank", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse([]));
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.spyOn>;
    const out = await apiGetWithTotal<unknown[]>("/inbox", "   ");
    expect(out.total).toBe(0);
    const headers = (fetchMock.mock.calls[0] as [string, RequestInit])[1].headers as Record<string, string>;
    expect(headers.Authorization).toBeUndefined();
  });

  it("degrades to total 0 when the header is missing or garbage", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse([]));
    expect((await apiGetWithTotal<unknown[]>("/inbox", "tok")).total).toBe(0);

    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse([], { headers: { "Content-Type": "application/json", "X-Total-Count": "nope" } }),
    );
    expect((await apiGetWithTotal<unknown[]>("/inbox", "tok")).total).toBe(0);
  });

  it("treats 204 as no items", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 204 }));
    const out = await apiGetWithTotal<unknown[] | undefined>("/inbox", "tok");
    expect(out.items).toBeUndefined();
    expect(out.total).toBe(0);
  });

  it("throws ApiError with the parsed message on failure", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse({ error: { message: "limit must be between 1 and 200" } }, { status: 400, statusText: "Bad Request" }),
    );
    await expect(apiGetWithTotal<unknown[]>("/inbox?limit=9999", "tok")).rejects.toThrow(
      "limit must be between 1 and 200",
    );
  });

  it("falls back to status text when the error body is not JSON", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("gateway sad", { status: 502, statusText: "Bad Gateway" }),
    );
    await expect(apiGetWithTotal<unknown[]>("/inbox", "tok")).rejects.toThrow(ApiError);
    await expect(apiGetWithTotal<unknown[]>("/inbox", "tok")).rejects.toThrow("Bad Gateway");
  });
});

describe("apiUploadImage", () => {
  const file = () => new File([new Uint8Array([1, 2, 3])], "shot.png", { type: "image/png" });

  it("posts multipart form with bearer token and returns the upload ref", async () => {
    const ref = { id: "up1", url: "/uploads/up1", content_type: "image/png", size: 3 };
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(ref));

    const out = await apiUploadImage("/uploads", " tok ", file(), "?owner=1");

    expect(out).toEqual(ref);
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(call[0]).toBe("/api/v1/uploads?owner=1");
    expect(call[1].method).toBe("POST");
    const headers = call[1].headers as Record<string, string>;
    expect(headers.Accept).toBe("application/json");
    expect(headers.Authorization).toBe("Bearer tok");
    expect(headers["Content-Type"]).toBeUndefined();
    const form = call[1].body as FormData;
    expect(form).toBeInstanceOf(FormData);
    expect((form.get("file") as File).name).toBe("shot.png");
  });

  it("omits Authorization when token is empty (OIDC cookie mode)", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse({ id: "u" }));
    await apiUploadImage("/uploads", "", file());
    const headers = (fetchMock.mock.calls[0] as [string, RequestInit])[1].headers as Record<string, string>;
    expect(headers.Authorization).toBeUndefined();
  });

  it("surfaces the parsed error message on failure", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse({ error: { code: "too_large", message: "image exceeds limit" } }, { status: 413, statusText: "Payload Too Large" }),
    );
    await expect(apiUploadImage("/uploads", "tok", file())).rejects.toMatchObject({
      status: 413,
      message: "image exceeds limit",
    } satisfies Partial<ApiError>);
  });

  it("falls back to status text when the error body is not JSON", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("nope", { status: 502, statusText: "Bad Gateway" }));
    await expect(apiUploadImage("/uploads", "tok", file())).rejects.toThrow("Bad Gateway");
  });
});

describe("extractErrorMessage fallbacks", () => {
  it("uses a top-level string message when error.message is absent", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse({ message: "top-level boom" }, { status: 400, statusText: "Bad Request" }));
    await expect(apiGet("/x", "")).rejects.toThrow("top-level boom");
  });

  it("falls back to status text when neither message field is a string", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse({ error: { code: 42 } }, { status: 422, statusText: "Unprocessable" }));
    await expect(apiGet("/x", "")).rejects.toThrow("Unprocessable");
  });
});
