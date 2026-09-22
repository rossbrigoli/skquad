// Server-only OIDC helpers (UIv2-13). Generic authorization-code + PKCE flow:
// everything comes from the IdP's .well-known/openid-configuration, so any
// compliant provider (Dex, Keycloak, Entra, Authelia…) works unchanged.
//
// Env (set on the web-v2 deployment):
//   SKQUAD_OIDC_ISSUER        e.g. https://cloud.rossbrigoli.com/auth
//   SKQUAD_OIDC_CLIENT_ID     e.g. skquad-v2
//   SKQUAD_OIDC_CLIENT_SECRET
//   SKQUAD_OIDC_REDIRECT_URL  e.g. https://skquad-v2.rossbrigoli.com/auth/callback
//   SKQUAD_OIDC_SCOPES        default "openid profile email offline_access"

import { createRemoteJWKSet, jwtVerify } from "jose";
import { createHash, randomBytes } from "node:crypto";

export type OidcConfig = {
  issuer: string;
  clientId: string;
  clientSecret: string;
  redirectUrl: string;
  scopes: string;
};

export type OidcDiscovery = {
  authorization_endpoint: string;
  token_endpoint: string;
  jwks_uri: string;
  end_session_endpoint?: string;
  [key: string]: unknown;
};

export function oidcEnabled(): boolean {
  return Boolean(
    process.env.SKQUAD_OIDC_ISSUER &&
      process.env.SKQUAD_OIDC_CLIENT_ID &&
      process.env.SKQUAD_OIDC_CLIENT_SECRET &&
      process.env.SKQUAD_OIDC_REDIRECT_URL,
  );
}

export function oidcConfig(): OidcConfig {
  const cfg = {
    issuer: (process.env.SKQUAD_OIDC_ISSUER || "").replace(/\/$/, ""),
    clientId: process.env.SKQUAD_OIDC_CLIENT_ID || "",
    clientSecret: process.env.SKQUAD_OIDC_CLIENT_SECRET || "",
    redirectUrl: process.env.SKQUAD_OIDC_REDIRECT_URL || "",
    scopes: process.env.SKQUAD_OIDC_SCOPES || "openid profile email offline_access",
  };
  if (!cfg.issuer || !cfg.clientId || !cfg.clientSecret || !cfg.redirectUrl) {
    throw new Error("OIDC is not fully configured");
  }
  return cfg;
}

export function randomUrlToken(bytes = 32): string {
  return randomBytes(bytes).toString("base64url");
}

export async function pkcePair(): Promise<{ verifier: string; challenge: string }> {
  const verifier = randomBytes(48).toString("base64url");
  const challenge = createHash("sha256").update(verifier).digest("base64url");
  return { verifier, challenge };
}

const DISCOVERY_TTL_MS = 60 * 60 * 1000;
let discoveryCache: { issuer: string; config: OidcDiscovery; fetchedAt: number } | null = null;

export async function discover(issuer: string): Promise<OidcDiscovery> {
  if (discoveryCache && discoveryCache.issuer === issuer && Date.now() - discoveryCache.fetchedAt < DISCOVERY_TTL_MS) {
    return discoveryCache.config;
  }
  const url = `${issuer}/.well-known/openid-configuration`;
  const res = await fetch(url, { cache: "no-store" });
  if (!res.ok) {
    throw new Error(`OIDC discovery failed (${res.status}) from ${url}`);
  }
  const config = (await res.json()) as OidcDiscovery;
  if (!config.authorization_endpoint || !config.token_endpoint || !config.jwks_uri) {
    throw new Error(`OIDC discovery document at ${url} is missing required endpoints`);
  }
  discoveryCache = { issuer, config, fetchedAt: Date.now() };
  return config;
}

const jwksCache = new Map<string, ReturnType<typeof createRemoteJWKSet>>();

export async function verifyIdToken(idToken: string, nonce: string): Promise<Record<string, unknown>> {
  const cfg = oidcConfig();
  const disc = await discover(cfg.issuer);
  let jwks = jwksCache.get(disc.jwks_uri);
  if (!jwks) {
    jwks = createRemoteJWKSet(new URL(disc.jwks_uri));
    jwksCache.set(disc.jwks_uri, jwks);
  }
  const { payload } = await jwtVerify(idToken, jwks, {
    issuer: cfg.issuer,
    audience: cfg.clientId,
  });
  if (payload.nonce !== nonce) {
    throw new Error("ID token nonce mismatch");
  }
  return payload as Record<string, unknown>;
}

export type TokenSet = {
  access_token: string;
  id_token?: string;
  refresh_token?: string;
  expires_in?: number;
  [key: string]: unknown;
};

export async function exchangeCode(code: string, verifier: string): Promise<TokenSet> {
  const cfg = oidcConfig();
  const disc = await discover(cfg.issuer);
  const body = new URLSearchParams({
    grant_type: "authorization_code",
    code,
    client_id: cfg.clientId,
    client_secret: cfg.clientSecret,
    redirect_uri: cfg.redirectUrl,
    code_verifier: verifier,
  });
  const res = await fetch(disc.token_endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" },
    body: body.toString(),
    cache: "no-store",
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(`token exchange failed (${res.status}): ${(json as { error_description?: string }).error_description || "unknown"}`);
  }
  return json as TokenSet;
}

export async function refreshTokens(refreshToken: string): Promise<TokenSet> {
  const cfg = oidcConfig();
  const disc = await discover(cfg.issuer);
  const body = new URLSearchParams({
    grant_type: "refresh_token",
    client_id: cfg.clientId,
    client_secret: cfg.clientSecret,
    refresh_token: refreshToken,
  });
  const res = await fetch(disc.token_endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" },
    body: body.toString(),
    cache: "no-store",
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(`refresh failed (${res.status})`);
  }
  return json as TokenSet;
}

// --- Session cookie model -----------------------------------------------------
// Which token do we present to the control-plane?
// Dex issues OPAQUE access tokens (no JWT structure, no introspection endpoint),
// while the control-plane authenticates humans with a go-oidc *ID-token* verifier
// keyed on audience == client_id. Dex ID tokens are signed JWTs with
// aud = <client_id>, iss = <dex issuer> — exactly what that verifier accepts.
// So the upstream bearer is the ID token, not the access token (UIv2-13 fix).
// Ref: https://dexidp.io/docs/configuration/tokens/  (ID tokens are JWTs; "aud" = the client id)
//      https://dexidp.io/docs/openid-connect/         (access tokens are opaque references)
export type Session = {
  access_token: string;
  id_token?: string;
  refresh_token?: string;
  expiry: number; // epoch seconds
  name?: string;
  email?: string;
};

// apiBearer is the credential the control-plane will actually verify.
export function apiBearer(s: Session): string {
  return s.id_token || s.access_token;
}

export const SESSION_COOKIE = "skquad_v2_session";
export const STATE_COOKIE = "skquad_oidc_state";
export const VERIFIER_COOKIE = "skquad_oidc_verifier";
export const NONCE_COOKIE = "skquad_oidc_nonce";

export function sessionCookieOpts() {
  const secure = oidcEnabled() && oidcConfig().redirectUrl.startsWith("https://");
  return {
    httpOnly: true,
    sameSite: "lax" as const,
    secure,
    path: "/",
  };
}

export function encodeSession(s: Session): string {
  return Buffer.from(JSON.stringify(s), "utf8").toString("base64url");
}

export function decodeSession(raw: string | undefined): Session | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(Buffer.from(raw, "base64url").toString("utf8"));
    const bearer = typeof parsed?.id_token === "string" ? parsed.id_token : parsed?.access_token;
    if (typeof bearer === "string" && bearer !== "" && typeof parsed?.expiry === "number") {
      return parsed as Session;
    }
    return null;
  } catch {
    return null;
  }
}

export function sessionValid(s: Session | null): boolean {
  return !!s && s.expiry > Math.floor(Date.now() / 1000) + 5;
}
