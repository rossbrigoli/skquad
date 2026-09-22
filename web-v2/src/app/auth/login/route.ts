import { NextResponse } from "next/server";
import { discover, oidcConfig, oidcEnabled, pkcePair, randomUrlToken, sessionCookieOpts } from "../../../lib/oidcServer";

// GET /auth/login → redirect to the IdP authorization endpoint (code + PKCE).
export async function GET() {
  if (!oidcEnabled()) {
    return NextResponse.json({ error: { message: "OIDC login is not enabled" } }, { status: 404 });
  }
  try {
    const cfg = oidcConfig();
    const disc = await discover(cfg.issuer);
    const state = randomUrlToken(24);
    const nonce = randomUrlToken(24);
    const { verifier, challenge } = await pkcePair();

    const url = new URL(disc.authorization_endpoint);
    url.searchParams.set("response_type", "code");
    url.searchParams.set("client_id", cfg.clientId);
    url.searchParams.set("redirect_uri", cfg.redirectUrl);
    url.searchParams.set("scope", cfg.scopes);
    url.searchParams.set("state", state);
    url.searchParams.set("nonce", nonce);
    url.searchParams.set("code_challenge", challenge);
    url.searchParams.set("code_challenge_method", "S256");

    const res = NextResponse.redirect(url.toString());
    const opts = { ...sessionCookieOpts(), maxAge: 600, path: "/auth" };
    res.cookies.set("skquad_oidc_state", state, opts);
    res.cookies.set("skquad_oidc_verifier", verifier, opts);
    res.cookies.set("skquad_oidc_nonce", nonce, opts);
    return res;
  } catch (err) {
    return NextResponse.json(
      { error: { message: err instanceof Error ? err.message : "login init failed" } },
      { status: 502 },
    );
  }
}
