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
    // Fail closed: the control-plane authenticates human requests by verifying the
    // Dex ID-token JWT (opaque access tokens cannot be verified there), so a login
    // that produced no id_token can never authorize an API call.
    if (!tokens.id_token) {
      throw new Error("token response contained no id_token — cannot establish an authenticated session");
    }
    const claims = await verifyIdToken(tokens.id_token, nonce);
    const name: string | undefined =
      typeof claims.name === "string"
        ? claims.name
        : typeof claims.preferred_username === "string"
          ? claims.preferred_username
          : undefined;
    const email: string | undefined = typeof claims.email === "string" ? claims.email : undefined;
    // The session must end no later than the credential we actually present
    // upstream, so take the tighter of the OAuth `expires_in` and the ID token's
    // own `exp`. Otherwise we could keep sending a JWT the control-plane rejects.
    const oauthExpiry = Math.floor(Date.now() / 1000) + (tokens.expires_in || 300);
    const idTokenExpiry = typeof claims.exp === "number" ? claims.exp : oauthExpiry;
    const expiry = Math.min(oauthExpiry, idTokenExpiry);
    const res = NextResponse.redirect(new URL("/", request.url));
    res.cookies.set(
      SESSION_COOKIE,
      encodeSession({
        access_token: tokens.access_token,
        id_token: tokens.id_token,
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
