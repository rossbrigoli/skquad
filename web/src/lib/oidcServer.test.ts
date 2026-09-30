import { describe, it, expect, afterEach, vi } from "vitest";
import {
  appRedirect,
  apiBearer,
  decodeSession,
  discover,
  encodeSession,
  exchangeCode,
  oidcConfig,
  oidcEnabled,
  refreshTokens,
  publicOrigin,
  requestIsHttps,
  sessionCookieOpts,
  sessionValid,
  type Session,
} from "./oidcServer";

const OIDC_ENV = {
  SKQUAD_OIDC_ISSUER: "https://idp.example.com/auth",
  SKQUAD_OIDC_CLIENT_ID: "skquad",
  SKQUAD_OIDC_CLIENT_SECRET: "***",
  SKQUAD_OIDC_REDIRECT_URL: "https://skquad.rossbrigoli.com/auth/callback",
};

function clearEnv() {
  for (const k of [
    "SKQUAD_PUBLIC_BASE_URL",
    ...Object.keys(OIDC_ENV),
  ]) {
    delete process.env[k];
  }
}

afterEach(clearEnv);
afterEach(() => {
  vi.restoreAllMocks();
});

// UIv2-13 fix: the control-plane verifies Dex *ID-token* JWTs (Dex access tokens
// are opaque), so the session must be able to carry and prefer the id_token.

describe("apiBearer", () => {
  it("prefers the ID token over the access token", () => {
    const s: Session = {
      access_token: "opaque-access",
      id_token: "jwt-id-token",
      expiry: 9_000_000_000,
    };
    expect(apiBearer(s)).toBe("jwt-id-token");
  });

  it("falls back to the access token when no ID token is stored", () => {
    const s: Session = { access_token: "opaque-access", expiry: 9_000_000_000 };
    expect(apiBearer(s)).toBe("opaque-access");
  });
});

describe("session cookie encode/decode", () => {
  it("round-trips a full session", () => {
    const s: Session = {
      access_token: "a",
      id_token: "b",
      refresh_token: "c",
      expiry: 123_456,
      name: "Ross",
      email: "ross@example.com",
    };
    expect(decodeSession(encodeSession(s))).toEqual(s);
  });

  it("accepts a session carrying only an ID token", () => {
    const raw = Buffer.from(
      JSON.stringify({ id_token: "b", expiry: 123_456 }),
      "utf8",
    ).toString("base64url");
    const s = decodeSession(raw);
    expect(s).not.toBeNull();
    expect(apiBearer(s as Session)).toBe("b");
  });

  it("rejects payloads with no usable bearer", () => {
    const raw = Buffer.from(JSON.stringify({ expiry: 123_456 }), "utf8").toString(
      "base64url",
    );
    expect(decodeSession(raw)).toBeNull();
  });

  it("rejects payloads with a non-numeric expiry", () => {
    const raw = Buffer.from(
      JSON.stringify({ id_token: "b", expiry: "soon" }),
      "utf8",
    ).toString("base64url");
    expect(decodeSession(raw)).toBeNull();
  });

  it("rejects garbage that is not base64url JSON", () => {
    expect(decodeSession("not-a-session")).toBeNull();
    expect(decodeSession(undefined)).toBeNull();
    expect(decodeSession("")).toBeNull();
  });

  it("is not encryption: base64url is reversible (documented limitation)", () => {
    const s: Session = { access_token: "a", id_token: "b", expiry: 1 };
    // httpOnly keeps it away from JS; it is not confidential at rest.
    expect(JSON.parse(Buffer.from(encodeSession(s), "base64url").toString("utf8")).id_token).toBe("b");
  });
});

describe("sessionValid", () => {  const now = () => Math.floor(Date.now() / 1000);

  it("is true comfortably inside the lifetime", () => {
    expect(sessionValid({ access_token: "a", expiry: now() + 60 })).toBe(true);
  });

  it("is false inside the 5s safety margin", () => {
    expect(sessionValid({ access_token: "a", expiry: now() + 2 })).toBe(false);
  });

  it("is false once expired", () => {
    expect(sessionValid({ access_token: "a", expiry: now() - 1 })).toBe(false);
  });

  it("is false for a null session", () => {
    expect(sessionValid(null)).toBe(false);
  });
});

describe("OIDC configuration helpers", () => {
  it("reports disabled until all required settings are present", () => {
    expect(oidcEnabled()).toBe(false);
    process.env.SKQUAD_OIDC_ISSUER = OIDC_ENV.SKQUAD_OIDC_ISSUER;
    process.env.SKQUAD_OIDC_CLIENT_ID = OIDC_ENV.SKQUAD_OIDC_CLIENT_ID;
    process.env.SKQUAD_OIDC_CLIENT_SECRET = OIDC_ENV.SKQUAD_OIDC_CLIENT_SECRET;
    expect(oidcEnabled()).toBe(false);
  });

  it("normalizes a complete config and applies default scopes", () => {
    Object.assign(process.env, OIDC_ENV);
    process.env.SKQUAD_OIDC_ISSUER = "https://idp.example.com/auth/";
    expect(oidcEnabled()).toBe(true);
    expect(oidcConfig()).toEqual({
      issuer: "https://idp.example.com/auth",
      clientId: "skquad",
      clientSecret: "***",
      redirectUrl: "https://skquad.rossbrigoli.com/auth/callback",
      scopes: "openid profile email offline_access",
    });
  });

  it("throws when config is incomplete", () => {
    expect(() => oidcConfig()).toThrow("OIDC is not fully configured");
  });

  it("sets secure session cookies only for public https redirects", () => {
    Object.assign(process.env, OIDC_ENV);
    expect(sessionCookieOpts()).toMatchObject({ httpOnly: true, sameSite: "lax", secure: true, path: "/" });

    process.env.SKQUAD_OIDC_REDIRECT_URL = "http://skquad.lab/auth/callback";
    expect(sessionCookieOpts().secure).toBe(false);
  });
});

describe("requestIsHttps", () => {
  it("trusts x-forwarded-proto first", () => {
    const req = new Request("http://internal", { headers: { "x-forwarded-proto": "https,http" } });
    expect(requestIsHttps(req)).toBe(true);
  });

  it("falls back to the request URL protocol", () => {
    expect(requestIsHttps(new Request("https://skquad.example.test/path"))).toBe(true);
    expect(requestIsHttps(new Request("http://skquad.example.test/path"))).toBe(false);
  });

  it("returns false when no usable request URL exists", () => {
    expect(requestIsHttps()).toBe(false);
  });
});

describe("discovery and token endpoints", () => {
  const discovery = {
    authorization_endpoint: "https://idp.example.test/auth",
    token_endpoint: "https://idp.example.test/token",
    jwks_uri: "https://idp.example.test/jwks",
  };

  it("discovers and caches provider metadata", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(discovery)));

    const first = await discover("https://idp-cache.example.test");
    const second = await discover("https://idp-cache.example.test");

    expect(first).toEqual(discovery);
    expect(second).toEqual(discovery);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("rejects failed and incomplete discovery documents", async () => {
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response("no", { status: 500 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ authorization_endpoint: "x" })));

    await expect(discover("https://idp-fail.example.test")).rejects.toThrow("OIDC discovery failed");
    await expect(discover("https://idp-incomplete.example.test")).rejects.toThrow("missing required endpoints");
  });

  it("exchanges authorization codes using PKCE form fields", async () => {
    Object.assign(process.env, OIDC_ENV);
    process.env.SKQUAD_OIDC_ISSUER = "https://idp-exchange.example.test";
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify(discovery)))
      .mockResolvedValueOnce(new Response(JSON.stringify({ access_token: "a", id_token: "i" })));

    const tokens = await exchangeCode("code-1", "verifier-1");

    expect(tokens).toMatchObject({ access_token: "a", id_token: "i" });
    const request = fetchMock.mock.calls[1][1] as RequestInit;
    expect(request.method).toBe("POST");
    expect(String(request.body)).toContain("code_verifier=verifier-1");
  });

  it("reports token endpoint failures", async () => {
    Object.assign(process.env, OIDC_ENV);
    process.env.SKQUAD_OIDC_ISSUER = "https://idp-token-fail.example.test";
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify(discovery)))
      .mockResolvedValueOnce(new Response(JSON.stringify({ error_description: "bad code" }), { status: 400 }));

    await expect(exchangeCode("bad", "verifier")).rejects.toThrow("bad code");
  });

  it("refreshes tokens and reports refresh failures", async () => {
    Object.assign(process.env, OIDC_ENV);
    process.env.SKQUAD_OIDC_ISSUER = "https://idp-refresh.example.test";
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify(discovery)))
      .mockResolvedValueOnce(new Response(JSON.stringify({ access_token: "new" })))
      .mockResolvedValueOnce(new Response(JSON.stringify({}), { status: 401 }));

    await expect(refreshTokens("refresh-1")).resolves.toMatchObject({ access_token: "new" });
    await expect(refreshTokens("refresh-2")).rejects.toThrow("refresh failed (401)");
  });
});

describe("publicOrigin (redirect-origin leak fix)", () => {
  const req = (headers: Record<string, string>) =>
    new Request("http://127.0.0.1:3000/auth/callback", { headers });

  it("explicit SKQUAD_PUBLIC_BASE_URL wins over everything", () => {
    process.env.SKQUAD_PUBLIC_BASE_URL = "https://skquad.rossbrigoli.com/";
    const o = publicOrigin(req({ "x-forwarded-host": "elsewhere.example.com" }));
    expect(o).toBe("https://skquad.rossbrigoli.com");
  });

  it("uses x-forwarded-host + x-forwarded-proto when no env is set", () => {
    const o = publicOrigin(
      req({ "x-forwarded-host": "skquad.rossbrigoli.com", "x-forwarded-proto": "https" }),
    );
    expect(o).toBe("https://skquad.rossbrigoli.com");
  });

  it("takes the first value of a comma-separated forwarded host chain", () => {
    const o = publicOrigin(
      req({ "x-forwarded-host": "skquad.rossbrigoli.com, internal.corp", "x-forwarded-proto": "https,http" }),
    );
    expect(o).toBe("https://skquad.rossbrigoli.com");
  });

  it("falls back to the Host header when there is no forwarded header", () => {
    const o = publicOrigin(req({ host: "skquad.rossbrigoli.com" }));
    expect(o).toBe("https://skquad.rossbrigoli.com");
  });

  it("refuses to echo a localhost Host (the exact bug we hit)", () => {
    process.env.SKQUAD_OIDC_REDIRECT_URL = "https://skquad.rossbrigoli.com/auth/callback";
    process.env.SKQUAD_OIDC_ISSUER = "https://idp.example.com/auth";
    process.env.SKQUAD_OIDC_CLIENT_ID = "c";
    process.env.SKQUAD_OIDC_CLIENT_SECRET = "s";
    const o = publicOrigin(req({ host: "localhost:3000" }));
    expect(o).not.toContain("localhost");
    expect(o).toBe("https://skquad.rossbrigoli.com");
  });

  it("refuses a 127.0.0.1 forwarded host too", () => {
    process.env.SKQUAD_OIDC_REDIRECT_URL = "https://skquad.rossbrigoli.com/auth/callback";
    process.env.SKQUAD_OIDC_ISSUER = "https://idp.example.com/auth";
    process.env.SKQUAD_OIDC_CLIENT_ID = "c";
    process.env.SKQUAD_OIDC_CLIENT_SECRET = "s";
    const o = publicOrigin(req({ "x-forwarded-host": "127.0.0.1:3000" }));
    expect(o).toBe("https://skquad.rossbrigoli.com");
  });

  it("returns empty string when nothing usable is configured", () => {
    expect(publicOrigin(req({ host: "localhost:3000" }))).toBe("");
    expect(publicOrigin()).toBe("");
  });
});

describe("appRedirect", () => {
  it("redirects through the public origin when available", () => {
    const res = appRedirect(
      new Request("http://127.0.0.1:3000", {
        headers: { "x-forwarded-host": "skquad.rossbrigoli.com", "x-forwarded-proto": "https" },
      }),
      "/squads/1",
    );
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://skquad.rossbrigoli.com/squads/1");
  });

  it("falls back to a relative redirect when no origin is known", () => {
    const res = appRedirect(undefined, "/login");
    expect(res.status).toBe(302);
    expect(res.headers.get("location")).toBe("/login");
  });
});
