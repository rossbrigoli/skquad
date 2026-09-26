import { NextRequest, NextResponse } from "next/server";
import {
  SESSION_COOKIE,
  appRedirect,
  discover,
  oidcConfig,
  oidcEnabled,
  sessionCookieOpts,
} from "../../../lib/oidcServer";

// GET /auth/logout — clear the session and (when the IdP supports it) end the
// SSO session at the provider too, then return to the app root.
export async function GET(_request: NextRequest) {
  const res = appRedirect(_request, "/");
  res.cookies.set(SESSION_COOKIE, "", { ...sessionCookieOpts(), maxAge: 0 });
  if (oidcEnabled()) {
    try {
      const cfg = oidcConfig();
      const disc = await discover(cfg.issuer);
      if (disc.end_session_endpoint) {
        const url = new URL(disc.end_session_endpoint);
        url.searchParams.set("client_id", cfg.clientId);
        const appBase = new URL(cfg.redirectUrl).origin;
        url.searchParams.set("post_logout_redirect_uri", appBase);
        return NextResponse.redirect(url.toString(), {
          headers: { "Set-Cookie": `${SESSION_COOKIE}=; ${cookieHeader(sessionCookieOpts())}` },
        });
      }
    } catch {
      // Best-effort: fall back to local-only logout.
    }
  }
  return res;
}

function cookieHeader(opts: { httpOnly: boolean; sameSite: "lax" | "strict" | "none"; secure: boolean; path: string }): string {
  const parts = ["Path=/", `SameSite=${opts.sameSite}`];
  if (opts.secure) parts.push("Secure");
  if (opts.httpOnly) parts.push("HttpOnly");
  parts.push("Max-Age=0");
  return parts.join("; ");
}
