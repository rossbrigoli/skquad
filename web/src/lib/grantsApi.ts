// TG-8 slice D: API client for the approval workflows. Thin wrappers over
// the shared api helpers (apiGet/apiPost/apiDelete) so the OIDC /proxy base
// and bearer auth are honoured exactly like every other surface (S-205
// posture — never a raw NEXT_PUBLIC base-URL fetch).
//
// Endpoint + body shapes verified against the live handlers:
//   control-plane/internal/httpapi/grant_requests.go (slice B)
//   control-plane/internal/httpapi/confirmations.go  (slice C1)
import { apiDelete, apiGet, apiPost } from "./api";
import type { GrantRequest, PendingConfirmation, StandingGrant } from "./grantsWorkflow";

// ── Grant requests (slice B) ─────────────────────────────────────────────

export function listGrantRequests(
  token: string,
  filter: { mine?: "owner" | "admin"; state?: string } = {},
): Promise<GrantRequest[]> {
  const params = new URLSearchParams();
  if (filter.mine) params.set("mine", filter.mine);
  if (filter.state) params.set("state", filter.state);
  const qs = params.toString();
  return apiGet<GrantRequest[]>(`/grant-requests${qs ? `?${qs}` : ""}`, token);
}

export function approveGrantOwner(token: string, id: string): Promise<GrantRequest> {
  return apiPost<GrantRequest>(`/grant-requests/${id}/approve-owner`, token, {});
}

export function approveGrantAdmin(token: string, id: string): Promise<GrantRequest> {
  return apiPost<GrantRequest>(`/grant-requests/${id}/approve-admin`, token, {});
}

export function denyGrantRequest(token: string, id: string, reason: string): Promise<GrantRequest> {
  return apiPost<GrantRequest>(`/grant-requests/${id}/deny`, token, { reason });
}

// ── Confirmations (slice C1) ─────────────────────────────────────────────

export function listConfirmations(
  token: string,
  filter: { mine?: boolean; state?: string } = {},
): Promise<PendingConfirmation[]> {
  const params = new URLSearchParams();
  if (filter.mine) params.set("mine", "true");
  if (filter.state) params.set("state", filter.state);
  const qs = params.toString();
  return apiGet<PendingConfirmation[]>(`/confirmations${qs ? `?${qs}` : ""}`, token);
}

export function approveConfirmationOnce(token: string, id: string): Promise<PendingConfirmation> {
  return apiPost<PendingConfirmation>(`/confirmations/${id}/approve-once`, token, {});
}

// approveConfirmationStanding posts an optional expiry (RFC3339). When the
// caller omits it the backend applies its own +90d default; the UI sends
// the picker value so the two agree on what the owner saw.
export function approveConfirmationStanding(
  token: string,
  id: string,
  expiryIso?: string,
): Promise<{ confirmation: PendingConfirmation; standing_grant: StandingGrant }> {
  const body = expiryIso ? { expiry: expiryIso } : {};
  return apiPost<{ confirmation: PendingConfirmation; standing_grant: StandingGrant }>(
    `/confirmations/${id}/approve-standing`,
    token,
    body,
  );
}

export function denyConfirmation(token: string, id: string, reason: string): Promise<PendingConfirmation> {
  return apiPost<PendingConfirmation>(`/confirmations/${id}/deny`, token, { reason });
}

// ── Standing grants (slice C1) ───────────────────────────────────────────

export function listStandingGrants(token: string): Promise<StandingGrant[]> {
  return apiGet<StandingGrant[]>("/standing-grants", token);
}

// apiDelete is void-typed by the shared helper; the revoked-grant JSON the
// handler returns is intentionally discarded (the panel refetches).
export function revokeStandingGrant(token: string, id: string): Promise<void> {
  return apiDelete(`/standing-grants/${id}`, token);
}
