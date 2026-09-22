import { NextResponse } from "next/server";
import { oidcEnabled } from "../../../lib/oidcServer";

// Public: tells the client which auth mode this deployment runs.
export async function GET() {
  return NextResponse.json({ mode: oidcEnabled() ? "oidc" : "token" });
}
