import { NextRequest, NextResponse } from "next/server";
import { SESSION_COOKIE, decodeSession, sessionValid } from "../../../lib/oidcServer";

// GET /auth/session — client-side probe: is there a valid UI session?
export async function GET(request: NextRequest) {
  const session = decodeSession(request.cookies.get(SESSION_COOKIE)?.value);
  if (!session || !sessionValid(session)) {
    return NextResponse.json({ authenticated: false }, { status: 401 });
  }
  return NextResponse.json({
    authenticated: true,
    name: session.name || "",
    email: session.email || "",
    expiry: session.expiry,
    can_refresh: Boolean(session.refresh_token),
  });
}
