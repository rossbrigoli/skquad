import { NextRequest, NextResponse } from "next/server";
import {
  SESSION_COOKIE,
  decodeSession,
  encodeSession,
  oidcEnabled,
  refreshTokens,
  sessionCookieOpts,
  sessionValid,
  type Session,
} from "../../../lib/oidcServer";

// /proxy/* — server-side forwarder to the control-plane API (UIv2-13).
// The access token never leaves the server: the browser authenticates to this
// route with the httpOnly session cookie, and we attach the bearer header
// upstream. Expired access tokens are refreshed transparently.

const UPSTREAM = (process.env.SKQUAD_API_BASE_URL || "http://localhost:8090/api/v1").replace(/\/$/, "");

async function liveSession(request: NextRequest): Promise<Session | null> {
  const session = decodeSession(request.cookies.get(SESSION_COOKIE)?.value);
  if (!session) return null;
  if (sessionValid(session)) return session;
  if (session.refresh_token && oidcEnabled()) {
    try {
      const tokens = await refreshTokens(session.refresh_token);
      const next: Session = {
        access_token: tokens.access_token,
        refresh_token: tokens.refresh_token || session.refresh_token,
        expiry: Math.floor(Date.now() / 1000) + (tokens.expires_in || 300),
        name: session.name,
        email: session.email,
      };
      return next;
      // Persist the refreshed session on the response via a header trick is not
      // possible here; callers set the cookie on their NextResponse instead.
      return next;
    } catch {
      return null;
    }
  }
  return null;
}

async function forward(request: NextRequest, method: string, path: string[]): Promise<NextResponse> {
  if (!oidcEnabled()) {
    return NextResponse.json(
      { error: { message: "proxy is only available in OIDC mode; use direct API tokens" } },
      { status: 501 },
    );
  }
  const session = await liveSession(request);
  if (!session) {
    return NextResponse.json({ error: { message: "session expired — please sign in again" } }, { status: 401 });
  }

  const search = new URL(request.url).search;
  const target = `${UPSTREAM}/${path.map(encodeURIComponent).join("/")}${search}`;
  const headers: Record<string, string> = {
    Accept: "application/json",
    Authorization: `Bearer ${session.access_token}`,
  };
  const body =
    method === "GET" || method === "HEAD" ? undefined : await request.text();
  if (body !== undefined) {
    headers["Content-Type"] = request.headers.get("Content-Type") || "application/json";
  }

  try {
    const upstream = await fetch(target, { method, headers, body, cache: "no-store" });
    const text = await upstream.text();
    const res = new NextResponse(text === "" ? null : text, {
      status: upstream.status,
      headers: { "Content-Type": upstream.headers.get("Content-Type") || "application/json" },
    });
    // If we refreshed, roll the new session into the response cookie.
    res.cookies.set(SESSION_COOKIE, encodeSession(session), { ...sessionCookieOpts(), maxAge: 8 * 3600 });
    return res;
  } catch (err) {
    return NextResponse.json(
      { error: { message: `upstream request failed: ${err instanceof Error ? err.message : "unknown"}` } },
      { status: 502 },
    );
  }
}

export async function GET(request: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return forward(request, "GET", (await ctx.params).path || []);
}
export async function POST(request: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return forward(request, "POST", (await ctx.params).path || []);
}
export async function PUT(request: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return forward(request, "PUT", (await ctx.params).path || []);
}
export async function PATCH(request: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return forward(request, "PATCH", (await ctx.params).path || []);
}
export async function DELETE(request: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return forward(request, "DELETE", (await ctx.params).path || []);
}
