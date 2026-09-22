import { describe, it, expect } from "vitest";
import {
  apiBearer,
  decodeSession,
  encodeSession,
  sessionValid,
  type Session,
} from "./oidcServer";

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

describe("sessionValid", () => {
  const now = () => Math.floor(Date.now() / 1000);

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
