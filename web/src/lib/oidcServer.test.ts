import { describe, it, expect, afterEach } from "vitest";
import {
  apiBearer,
  decodeSession,
  encodeSession,
  publicOrigin,
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
