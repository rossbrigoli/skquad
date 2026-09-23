import { NextResponse } from "next/server";

/**
 * GET /auth/breakglass/status
 *
 * Tells the login screen whether the break-glass form should be rendered.
 * Proxied server-side so the browser never learns the control-plane address.
 */
const UPSTREAM = (process.env.SKQUAD_API_BASE_URL || "http://localhost:8090/api/v1").replace(/\/$/, "");

export async function GET() {
  try {
    const res = await fetch(`${UPSTREAM}/auth/breakglass/status`, { cache: "no-store" });
    if (!res.ok) return NextResponse.json({ enabled: false });
    const body = (await res.json()) as { enabled?: boolean };
    return NextResponse.json({ enabled: body.enabled === true });
  } catch {
    // Control-plane unreachable: never show a form that cannot work.
    return NextResponse.json({ enabled: false });
  }
}
