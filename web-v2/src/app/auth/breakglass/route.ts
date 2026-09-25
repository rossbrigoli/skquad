import { NextResponse } from "next/server";

import {
  SESSION_COOKIE,
  encodeSession,
  requestIsHttps,
  sessionCookieOpts,
  type Session,
} from "../../../lib/oidcServer";

const UPSTREAM = (process.env.SKQUAD_API_BASE_URL || "http://localhost:8090/api/v1").replace(/\/$/, "");

/**
 * POST /auth/breakglass
 *
 * Server-side relay for the break-glass admin login. It exists so the browser
 * never holds the control-plane address and so the session cookie stays
 * httpOnly.
 *
 * IP forwarding matters here. The control-plane enforces "LAN/Tailscale only"
 * and refuses anything that came through Cloudflare, but it can only make that
 * judgement if we pass the original request's signals through:
 *
 *   - x-real-ip: set by Traefik from the actual TCP peer (Traefik overwrites
 *     any client-supplied value), so it is the client we should trust.
 *   - cf-ray / cf-connecting-ip: Cloudflare always stamps these on what it
 *     proxies. Forwarding them is what makes the control-plane reject a
 *     break-glass attempt made against the public hostname from inside the house.
 *
 * Dropping either of these would silently break the security boundary, so this
 * function is deliberately explicit about what it forwards.
 */
export async function POST(request: Request) {
  let payload: { username?: string; password?: string };
  try {
    payload = await request.json();
  } catch {
    return NextResponse.json({ error: "expected JSON body" }, { status: 400 });
  }

  const username = typeof payload.username === "string" ? payload.username : "";
  const password = typeof payload.password === "string" ? payload.password : "";
  if (!username || !password) {
    return NextResponse.json({ error: "username and password are required" }, { status: 400 });
  }

  const headers = new Headers({
    "Content-Type": "application/json",
    "User-Agent": "skquad-web-v2-breakglass",
  });
  for (const name of ["x-real-ip", "x-forwarded-for", "cf-ray", "cf-connecting-ip"]) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }

  let upstream: Response;
  try {
    upstream = await fetch(`${UPSTREAM}/auth/breakglass/login`, {
      method: "POST",
      headers,
      body: JSON.stringify({ username, password }),
      cache: "no-store",
    });
  } catch {
    return NextResponse.json({ error: "control-plane unreachable" }, { status: 502 });
  }

  const body = (await upstream.json().catch(() => ({}))) as {
    token?: string;
    expires_at?: string;
    user?: { id?: string; email?: string; name?: string; role?: string };
    error?: { message?: string } | string;
  };

  if (!upstream.ok || !body.token) {
    const message =
      typeof body.error === "string"
        ? body.error
        : body.error?.message || "break-glass login failed";
    return NextResponse.json({ error: message }, { status: upstream.status || 401 });
  }

  const expiresAt = body.expires_at ? Date.parse(body.expires_at) : Date.now() + 60 * 60 * 1000;
  const session: Session = {
    access_token: body.token,
    // Storing the break-glass token as id_token makes apiBearer() pick it, so the
    // existing proxy path works unchanged.
    id_token: body.token,
    expiry: Math.floor(expiresAt / 1000),
    name: body.user?.name || "break-glass",
    email: body.user?.email ?? "",
  };

  const res = NextResponse.json({ ok: true, user: body.user ?? null });
  // The shared sessionCookieOpts() derives `secure` from the OIDC redirect URL,
  // which is https://skquad-v2.rossbrigoli.com. Break-glass is reached over
  // plain-HTTP on the internal name (http://skquad-v2.lab) or over Tailscale,
  // so inheriting secure=true would mean the browser never sends the cookie back
  // and every proxied call comes back 401 "session expired".
  //
  // Follow the protocol the request actually arrived on instead. This weakens
  // cookie confidentiality on the LAN only, and only for the break-glass
  // session — which is already restricted to LAN/Tailscale by the control-plane.
  const secure = requestIsHttps(request);
  res.cookies.set(
    SESSION_COOKIE,
    encodeSession(session),
    { ...sessionCookieOpts(), secure, path: "/", maxAge: Math.max(60, Math.floor((expiresAt - Date.now()) / 1000)) },
  );
  return res;
}
