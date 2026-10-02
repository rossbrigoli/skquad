// S-200 follow-up #2: the proxy forwarded request bodies with
// `request.text()`, which UTF-8-decodes them and corrupts
// multipart/form-data image uploads (control-plane then rejects with
// "file field is required" / "only png, jpeg, gif and webp"). These
// tests pin the binary-safe passthrough behaviour.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

vi.mock("../../../lib/oidcServer", () => ({
  SESSION_COOKIE: "skquad_session",
  apiBearer: () => "bearer-token",
  decodeSession: (v: string | undefined) =>
    v === "valid"
      ? { access_token: "a", expiry: 9_999_999_999, name: "t", email: "e@x.io" }
      : null,
  encodeSession: () => "encoded",
  oidcEnabled: () => true,
  refreshTokens: async () => {
    throw new Error("not in this test");
  },
  requestIsHttps: () => false,
  sessionCookieOpts: () => ({}),
  sessionValid: () => true,
}));

import { POST } from "./route";

type Captured = { url?: string; body?: unknown; contentType?: string };
let captured: Captured;

beforeEach(() => {
  captured = {};
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
