import { NextRequest, NextResponse } from "next/server";
import {
  SESSION_COOKIE,
  decodeSession,
  encodeSession,
  exchangeCode,
  oidcEnabled,
  sessionCookieOpts,
  verifyIdToken,
} from "../../../lib/oidcServer";

// GET /auth/callback?code=..&state=.. — IdP redirect target (UIv2-13).
export async function GET(request: NextRequest) {
  if (!oidcEnabled()) {
    return NextResponse.json({ error: { message: "OIDC login is not enabled" } }, { status: 404 });
  }
  const params = new URL(request.url).searchParams;
  const code = params.get("code");
  const state = params.get("state");
  const idpError = params.get("error");

  if (idpError) {
    return NextResponse.json(
      { error: { message: `identity provider returned an error: ${idpError}` } },
      { status: 401 },
    );
  }
  if (!code || !state) {
    return NextResponse.json({ error: { message: "missing code or state" } }, { status: 400 });
  }

  const expectedState = request.cookies.get("skquad_oidc_state")?.value;
  const verifier = request.cookies.get("skquad_oidc_verifier")?.value;
  const nonce = request.cookies.get("skquad_oidc_nonce")?.value;
  if (!expectedState || !verifier || !nonce || expectedState !== state) {
    return NextResponse.json({ error: { message: "state mismatch — please retry the login" } }, { status: 401 });
  }

  try {
    const tokens = await exchangeCode(code, verifier);
    if (!tokens.access_token) {
      throw new Error("token response contained no access token");
    }
    let name: string | undefined;
    let email: string | undefined;
    if (tokens.id_token) {
      const claims = await verifyIdToken(tokens.id_token, nonce);
      name = typeof claims.name === "string" ? claims.name : typeof claims.preferred_username === "string" ? claims.preferred_username : undefined;
      email = typeof claims.email === "string" ? claims.email : undefined;
    }
    const expiry = Math.floor(Date.now() / 1000) + (tokens.expires_in || 300);
    const res = NextResponse.redirect(new URL("/", request.url));
    res.cookies.set(
      SESSION_COOKIE,
      encodeSession({
        access_token: tokens.access_token,
        refresh_token: tokens.refresh_token,
        expiry,
        name,
        email,
      }),
      { ...sessionCookieOpts(), maxAge: 8 * 3600 },
    );
    for (const c of ["skquad_oidc_state", "skquad_oidc_verifier", "skquad_oidc_nonce"]) {
      res.cookies.set(c, "", { ...sessionCookieOpts(), path: "/auth", maxAge: 0 });
    }
    return res;
  } catch (err) {
    return NextResponse.json(
      { error: { message: err instanceof Error ? err.message : "callback failed" } },
      { status: 502 },
    );
  }
}

// ensure decodeSession import is retained for future introspection use
void decodeSession;
