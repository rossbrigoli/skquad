// S-200 follow-up #2: the proxy forwarded request bodies with
// `request.text()`, which UTF-8-decodes them and corrupts
// multipart/form-data image uploads (control-plane then rejects with
// "file field is required" / "only png, jpeg, gif and webp"). These
// tests pin the binary-safe passthrough behaviour.
//
// S-226: the same class of bug on the RESPONSE side — `upstream.text()`
// corrupted binary responses, so attachment images fetched through the
// OIDC proxy (the only authed path for <img> bytes) rendered as broken.
// The GET tests below pin byte-exact response passthrough, JSON
// passthrough, query forwarding, and the 401/501 auth gates.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

const state = vi.hoisted(() => ({ oidcEnabled: true }));

vi.mock("../../../lib/oidcServer", () => ({
  SESSION_COOKIE: "skquad_session",
  apiBearer: () => "bearer-token",
  decodeSession: (v: string | undefined) =>
    v === "valid"
      ? { access_token: "a", expiry: 9_999_999_999, name: "t", email: "e@x.io" }
      : null,
  encodeSession: () => "encoded",
  oidcEnabled: () => state.oidcEnabled,
  refreshTokens: async () => {
    throw new Error("not in this test");
  },
  requestIsHttps: () => false,
  sessionCookieOpts: () => ({}),
  sessionValid: () => true,
}));

import { GET, POST } from "./route";

type Captured = { url?: string; body?: unknown; contentType?: string };
let captured: Captured;

beforeEach(() => {
  captured = {};
  state.oidcEnabled = true;
  vi.stubGlobal(
    "fetch",
    async (_url: string | URL | Request, init?: RequestInit) => {
      captured.url = String(_url);
      captured.body = init?.body;
      captured.contentType =
        (init?.headers as Record<string, string>)?.["Content-Type"] ?? undefined;
      return new Response(JSON.stringify({ id: "up1" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      });
    },
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function indexOfBytes(haystack: Uint8Array, needle: Uint8Array): number {
  outer: for (let i = 0; i + needle.length <= haystack.length; i++) {
    for (let j = 0; j < needle.length; j++) {
      if (haystack[i + j] !== needle[j]) continue outer;
    }
    return i;
  }
  return -1;
}

function postWithBody(body: BodyInit, contentType?: string) {
  const req = new NextRequest("http://localhost/proxy/uploads?squad_id=s1", {
    method: "POST",
    body,
  });
  if (contentType) req.headers.set("Content-Type", contentType);
  req.cookies.set("skquad_session", "valid");
  return POST(req, { params: Promise.resolve({ path: ["uploads"] }) });
}

describe("proxy multipart passthrough", () => {
  it("forwards multipart image bytes byte-for-byte (no UTF-8 corruption)", async () => {
    // PNG magic + deliberately invalid UTF-8 sequences — exactly what a
    // text() round-trip would mangle into U+FFFD.
    const payload = new Uint8Array([
      0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe, 0x00, 0x80,
      0xc3, 0x28, 0xff, 0xff,
    ]);
    const form = new FormData();
    form.append("file", new File([payload], "shot.png", { type: "image/png" }));

    const res = await postWithBody(form);
    expect(res.status).toBe(201);
    expect(captured.url).toContain("/uploads?squad_id=s1");

    const sent = new Uint8Array(captured.body as ArrayBuffer);
    expect(indexOfBytes(sent, payload)).toBeGreaterThanOrEqual(0);
    expect(captured.contentType ?? "").toMatch(/^multipart\/form-data; boundary=/);
  });

  it("still forwards JSON bodies unchanged", async () => {
    const res = await postWithBody(
      JSON.stringify({ message: "hi ✨" }),
      "application/json",
    );
    expect(res.status).toBe(201);
    const sent = new TextDecoder().decode(captured.body as ArrayBuffer);
    expect(sent).toBe(JSON.stringify({ message: "hi ✨" }));
  });
});

// S-226: response-side binary passthrough.
function getWithCookie(path: string[], query = "") {
  const req = new NextRequest(`http://localhost/proxy/${path.join("/")}${query}`, {
    method: "GET",
  });
  req.cookies.set("skquad_session", "valid");
  return GET(req, { params: Promise.resolve({ path }) });
}

// Records the upstream request like the beforeEach stub, but returns a
// caller-supplied response (for testing response passthrough).
function stubFetchWith(response: () => Response) {
  vi.stubGlobal(
    "fetch",
    async (url: string | URL | Request, init?: RequestInit) => {
      captured.url = String(url);
      captured.body = init?.body;
      captured.contentType =
        (init?.headers as Record<string, string>)?.["Content-Type"] ?? undefined;
      return response();
    },
  );
}

describe("proxy GET response passthrough (S-226)", () => {
  it("preserves non-UTF8 image bytes end-to-end", async () => {
    const png = new Uint8Array([
      0x89, 0x50, 0x4e, 0x47, 0xff, 0xfe, 0x00, 0x80, 0xc3, 0x28,
    ]);
    stubFetchWith(
      () => new Response(png, { status: 200, headers: { "Content-Type": "image/png" } }),
    );

    const res = await getWithCookie(["uploads", "abc"]);

    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("image/png");
    const out = new Uint8Array(await res.arrayBuffer());
    expect(out).toEqual(png);
    expect(captured.url).toContain("/uploads/abc");
  });

  it("attaches the bearer token upstream", async () => {
    let authHeader: string | undefined;
    vi.stubGlobal(
      "fetch",
      async (_url: string | URL | Request, init?: RequestInit) => {
        authHeader = (init?.headers as Record<string, string>)?.["Authorization"];
        return new Response("ok", { status: 200, headers: { "Content-Type": "text/plain" } });
      },
    );

    const res = await getWithCookie(["uploads", "abc"]);
    expect(res.status).toBe(200);
    expect(authHeader).toBe("Bearer bearer-token");
  });

  it("still passes JSON through unchanged", async () => {
    const body = { ok: true, items: [1, 2, 3] };
    stubFetchWith(
      () =>
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );

    const res = await getWithCookie(["squads"]);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual(body);
  });

  it("forwards the query string upstream", async () => {
    stubFetchWith(
      () => new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
    );

    await getWithCookie(["uploads"], "?squad_id=7");
    expect(captured.url).toContain("/uploads?squad_id=7");
  });

  it("returns 401 without a valid session and never calls upstream", async () => {
    const req = new NextRequest("http://localhost/proxy/uploads/abc", { method: "GET" });
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);

    const res = await GET(req, { params: Promise.resolve({ path: ["uploads", "abc"] }) });

    expect(res.status).toBe(401);
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("returns 501 when OIDC is disabled (proxy not applicable)", async () => {
    state.oidcEnabled = false;
    const res = await getWithCookie(["uploads", "abc"]);
    expect(res.status).toBe(501);
  });
});

// S-267: the inbox pager reads X-Total-Count via apiGetWithTotal(); the
// proxy must copy it from the upstream response or the pager is invisible
// in OIDC mode (total parses as 0). Explicit allowlist — only the headers
// the browser actually needs, never hop-by-hop/cookie headers.
describe("proxy X-Total-Count passthrough (S-267)", () => {
  it("copies X-Total-Count from upstream to the proxied response", async () => {
    const body = [{ id: "n1" }, { id: "n2" }];
    stubFetchWith(
      () =>
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json", "X-Total-Count": "42" },
        }),
    );

    const res = await getWithCookie(["inbox"], "?limit=25");
    expect(res.status).toBe(200);
    expect(res.headers.get("X-Total-Count")).toBe("42");
    expect(await res.json()).toEqual(body);
  });

  it("omits X-Total-Count when upstream does not send it", async () => {
    stubFetchWith(
      () => new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
    );

    const res = await getWithCookie(["squads"]);
    expect(res.status).toBe(200);
    expect(res.headers.get("X-Total-Count")).toBeNull();
  });

  it("keeps status and byte-exact body passthrough alongside the header", async () => {
    const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0xff, 0xfe]);
    stubFetchWith(
      () =>
        new Response(png, {
          status: 404,
          headers: { "Content-Type": "image/png", "X-Total-Count": "0" },
        }),
    );

    const res = await getWithCookie(["uploads", "missing"]);
    expect(res.status).toBe(404);
    expect(res.headers.get("X-Total-Count")).toBe("0");
    const out = new Uint8Array(await res.arrayBuffer());
    expect(out).toEqual(png);
  });

  it("does not leak other upstream headers (cookies, CORS, server)", async () => {
    stubFetchWith(
      () =>
        new Response("{}", {
          status: 200,
          headers: {
            "Content-Type": "application/json",
            "X-Total-Count": "7",
            "Set-Cookie": "session=leak; Path=/",
            "Access-Control-Allow-Origin": "*",
            Server: "upstream-secret",
          },
        }),
    );

    const res = await getWithCookie(["inbox"]);
    expect(res.headers.get("X-Total-Count")).toBe("7");
    // The proxy sets its OWN skquad_session cookie; what must not appear is
    // the upstream's Set-Cookie value leaking through.
    const setCookie = res.headers.get("Set-Cookie") ?? "";
    expect(setCookie).not.toContain("session=leak");
    expect(res.headers.get("Access-Control-Allow-Origin")).toBeNull();
    expect(res.headers.get("Server")).toBeNull();
  });
});
